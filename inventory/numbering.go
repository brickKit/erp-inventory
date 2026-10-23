package inventory

import (
	"context"
	"time"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/work"
)

// Limits of a numbering transaction (the same as the SDK's outbox numbering, P6.5).
var numberLimits = []db.TxOption{db.LockTimeout(2 * time.Second), db.TransactionTimeout(5 * time.Second)}

const numberBatch = 500

// counter locks a sequence's counter row and returns its last number in the database's current epoch; a new epoch
// restarts it (protocol 14: a restored or switched database numbers anew from 1).
func counter(ctx context.Context, tx db.Tx, scope, k string) (epoch string, last int64, err error) {
	if _, err = tx.Exec(ctx, `INSERT INTO seq_counter (scope, key) VALUES ($1, $2) ON CONFLICT DO NOTHING`, scope, k); err != nil {
		return
	}
	var stored *string
	if err = tx.QueryRow(ctx, `SELECT epoch, last_seq FROM seq_counter WHERE scope = $1 AND key = $2 FOR UPDATE`, scope, k).
		Scan(&stored, &last); err != nil {
		return
	}
	var now events.Epoch
	if err = tx.QueryRow(ctx, `SELECT generation, timeline FROM be_current_db_epoch()`).Scan(&now.Generation, &now.Timeline); err != nil {
		return
	}
	epoch = now.String()
	if stored == nil || *stored != epoch {
		last = 0
	}
	return
}

// numberInventory gives the visible movements without an inventory sequence their numbers, in insertion order, under
// the inventory counter's lock (protocol 02, step 2: the statement after the lock sees every committed row).
func (m *Module) numberInventory(ctx context.Context) (int, error) {
	var n int
	err := m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
		epoch, last, err := counter(ctx, tx, "inventory", "")
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `WITH picked AS (
				SELECT id, row_number() OVER (ORDER BY ord) AS rn FROM (
					SELECT id, ord FROM movement WHERE inventory_seq IS NULL ORDER BY ord LIMIT $1) p)
			UPDATE movement m SET inventory_seq = $2 + picked.rn, inventory_epoch = $3 FROM picked WHERE m.id = picked.id`,
			numberBatch, last, epoch)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		_, err = tx.Exec(ctx, `UPDATE seq_counter SET epoch = $1, last_seq = $2 WHERE scope = 'inventory' AND key = ''`,
			epoch, last+int64(n))
		return err
	}, numberLimits...)
	return n, err
}

// numberValuation gives the movements that have an inventory sequence their valuation sequence per product and legal
// entity (protocol 05b), and writes each movement's event in that transaction: the event carries both sequences.
func (m *Module) numberValuation(ctx context.Context) (int, error) {
	var n int
	err := m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
		n = 0
		rows, err := tx.Query(ctx, `SELECT DISTINCT product_id::text, legal_entity_id::text FROM movement
			WHERE inventory_seq IS NOT NULL AND valuation_seq IS NULL ORDER BY 1, 2 LIMIT 50`)
		if err != nil {
			return err
		}
		var groups [][2]string
		for rows.Next() {
			var g [2]string
			if err := rows.Scan(&g[0], &g[1]); err != nil {
				rows.Close()
				return err
			}
			groups = append(groups, g)
		}
		rows.Close()
		for _, g := range groups { // counters locked in (product, legal entity) order
			epoch, last, err := counter(ctx, tx, "valuation", g[0]+"/"+g[1])
			if err != nil {
				return err
			}
			mrows, err := tx.Query(ctx, `WITH picked AS (
					SELECT id, row_number() OVER (ORDER BY inventory_epoch, inventory_seq) AS rn FROM (
						SELECT id, inventory_epoch, inventory_seq FROM movement WHERE product_id = $1 AND legal_entity_id = $2
						AND inventory_seq IS NOT NULL AND valuation_seq IS NULL ORDER BY inventory_epoch, inventory_seq LIMIT $3) p)
				UPDATE movement m SET valuation_seq = $4 + picked.rn, valuation_epoch = $5 FROM picked WHERE m.id = picked.id
				RETURNING m.id::text`, g[0], g[1], numberBatch, last, epoch)
			if err != nil {
				return err
			}
			var ids []string
			for mrows.Next() {
				var id string
				if err := mrows.Scan(&id); err != nil {
					mrows.Close()
					return err
				}
				ids = append(ids, id)
			}
			mrows.Close()
			if _, err := tx.Exec(ctx, `UPDATE seq_counter SET epoch = $1, last_seq = $2 WHERE scope = 'valuation' AND key = $3`,
				epoch, last+int64(len(ids)), g[0]+"/"+g[1]); err != nil {
				return err
			}
			for _, id := range ids {
				mv, err := one(tx.QueryRow(ctx, `SELECT `+movementCols+` FROM movement m WHERE m.id = $1::uuid`, id), scanMovement)
				if err != nil {
					return err
				}
				if _, err := m.rt.Events.Emit(ctx, tx, Stream, events.Event{Type: MovementRecorded, RecordID: mv.FactID,
					Version: 1, Projection: events.Full, Data: mv, LegalEntity: mv.LegalEntityID}); err != nil {
					return err
				}
			}
			n += len(ids)
		}
		return nil
	}, numberLimits...)
	return n, err
}

// number runs both numberings until nothing is left.
func (m *Module) number(ctx context.Context) error {
	for {
		a, err := m.numberInventory(ctx)
		if err != nil {
			return err
		}
		b, err := m.numberValuation(ctx)
		if err != nil {
			return err
		}
		if a == 0 && b == 0 {
			return nil
		}
	}
}

// numberNow numbers right after a commit so the movement events go out at once; a failure is left to the numbering
// job, which runs every second.
func (m *Module) numberNow(ctx context.Context) {
	if err := m.number(context.WithoutCancel(ctx)); err != nil {
		m.rt.Logger.Warn("numbering movements after commit failed; the numbering job retries", "error", err)
	}
}

// Jobs declares the background work: the numbering of movements and the expiry of holds (protocol 01).
func (m *Module) Jobs(j *work.Jobs) error {
	j.Add(work.Job{Name: "number-movements", Kind: work.Singleton, Effect: work.Local, Interval: time.Second,
		MaxAttempts: work.Unlimited, Handle: func(ctx context.Context, _ *work.Run) error { return m.number(ctx) }})
	j.Add(work.Job{Name: "expire-holds", Kind: work.Singleton, Effect: work.Local, Interval: 10 * time.Second,
		MaxAttempts: work.Unlimited, Handle: m.expireHolds})
	return nil
}

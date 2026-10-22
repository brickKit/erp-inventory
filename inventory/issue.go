package inventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/grpcx"
	"github.com/brickKit/be-sdk-go/v3/ids"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	"google.golang.org/protobuf/proto"
)

// refused carries the problems of an issue out of its transaction, which then rolls back: nothing is written.
type refused struct{ problems []*inventoryv3.IssueProblem }

func (r *refused) Error() string { return fmt.Sprintf("the issue is refused: %d problems", len(r.problems)) }

// allocation is one movement an issue line becomes: a batch-tracked line may take several batches (FIFO).
type allocation struct {
	line     int
	batch    *string
	quantity string
}

// Issue writes the issue movements and consumes the branch in one transaction (RS19); it is idempotent by
// consumption_id, and the whole issue is refused when any line cannot be issued.
func (m *Module) Issue(ctx context.Context, in *inventoryv3.IssueRequest) (*inventoryv3.IssueResponse, error) {
	if in.GetConsumptionId() == "" || len(in.GetConsumptionId()) > 128 {
		return nil, invalid("consumption_id", "consumption_id is the coordinator's delivery ID")
	}
	if err := branchID(in.GetBranchId()); err != nil {
		return nil, err
	}
	if !dateForm.MatchString(in.GetBusinessDate()) {
		return nil, invalid("business_date", "business_date is YYYY-MM-DD")
	}
	if len(in.GetLines()) == 0 || len(in.GetLines()) > MaxBatch {
		return nil, invalid("lines", fmt.Sprintf("1-%d lines", MaxBatch))
	}
	var pids []string
	qty := make([]string, len(in.GetLines()))
	for i, l := range in.GetLines() {
		if !uuidForm.MatchString(l.GetProductId()) || !uuidForm.MatchString(l.GetWarehouseId()) {
			return nil, invalid(fmt.Sprintf("lines[%d]", i), "product_id and warehouse_id are IDs")
		}
		q, err := quantity(fmt.Sprintf("lines[%d].quantity", i), l.GetQuantity())
		if err != nil {
			return nil, err
		}
		qty[i] = q
		pids = append(pids, l.GetProductId())
	}
	body, _ := proto.MarshalOptions{Deterministic: true}.Marshal(in)
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	products, perr := m.productsOf(ctx, pids)
	caller, _ := grpcx.Caller(ctx)
	var resp *inventoryv3.IssueResponse
	err := m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO consumption (id, branch_id, content_digest, movement_ids)
			VALUES ($1, $2, $3, '{}') ON CONFLICT (id) DO NOTHING`, in.GetConsumptionId(), in.GetBranchId(), digest)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 { // issued before: the same answer, or a conflict
			var d string
			var moves []string
			if err := tx.QueryRow(ctx, `SELECT content_digest, movement_ids::text[] FROM consumption WHERE id = $1`,
				in.GetConsumptionId()).Scan(&d, &moves); err != nil {
				return err
			}
			if d != digest {
				return errContent.New("consumption " + in.GetConsumptionId() + " was issued with other content")
			}
			b, err := getBranch(ctx, tx, in.GetBranchId(), false)
			if err != nil {
				return err
			}
			resp = &inventoryv3.IssueResponse{ConsumptionId: in.GetConsumptionId(), Issued: true, MovementIds: moves,
				BranchState: branchStates[b.State]}
			return nil
		}
		if perr != nil {
			return perr
		}
		var problems []*inventoryv3.IssueProblem
		problem := func(i int, reason, detail string) {
			problems = append(problems, &inventoryv3.IssueProblem{Line: int32(i), Reason: reason, Detail: detail})
		}
		b, err := getBranch(ctx, tx, in.GetBranchId(), true)
		if errors.Is(err, errNone) || err == nil && b.State != "committed" {
			problem(0, "branch_not_committed", "the branch is not committed: "+b.State)
			return &refused{problems}
		}
		if err != nil {
			return err
		}
		// the branch's remaining quantity per product and warehouse, less what earlier lines of this issue take
		remaining := map[key]string{}
		for _, l := range b.Lines {
			var r string
			if err := tx.QueryRow(ctx, `SELECT trim_scale($1::numeric - $2::numeric)::text`, l.Quantity, l.Consumed).
				Scan(&r); err != nil {
				return err
			}
			remaining[key{l.ProductID, l.WarehouseID}] = r
		}
		keys := make([]key, len(in.GetLines()))
		for i, l := range in.GetLines() {
			keys[i] = key{l.GetProductId(), l.GetWarehouseId()}
		}
		if _, err := lockPositions(ctx, tx, *b.LegalEntityID, keys); err != nil {
			return err
		}
		var allocs []allocation
		for i, l := range in.GetLines() {
			k := keys[i]
			rest, ok := remaining[k]
			enough := false
			if ok {
				if enough, err = atLeast(ctx, tx, rest, qty[i]); err != nil {
					return err
				}
			}
			if !enough {
				problem(i, "exceeds_branch", "more than the branch has left for this product and warehouse")
				continue
			}
			if err := tx.QueryRow(ctx, `SELECT trim_scale($1::numeric - $2::numeric)::text`, rest, qty[i]).
				Scan(&rest); err != nil {
				return err
			}
			remaining[k] = rest
			p := products[l.GetProductId()]
			if err := checkTracking("", p, qty[i], l.GetBatch(), l.GetSerials(), false); err != nil {
				reason := "serial_count_mismatch"
				if p.Tracking == "serial" && len(l.GetSerials()) == 0 {
					reason = "serial_required"
				} else if p.Tracking != "serial" {
					reason = "tracking_mismatch"
				}
				problem(i, reason, err.Error())
				continue
			}
			switch {
			case p.Tracking == "serial":
				var bad []string
				if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(s ORDER BY s), '{}') FROM unnest($2::text[]) s
					WHERE NOT EXISTS (SELECT 1 FROM stock_serial t WHERE t.product_id = $1 AND t.serial = s)`,
					l.GetProductId(), l.GetSerials()).Scan(&bad); err != nil {
					return err
				}
				if len(bad) > 0 {
					problem(i, "serial_not_found", fmt.Sprint(bad))
					continue
				}
				if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(serial ORDER BY serial), '{}') FROM stock_serial
					WHERE product_id = $1 AND serial = ANY($2) AND NOT in_stock`, l.GetProductId(), l.GetSerials()).
					Scan(&bad); err != nil {
					return err
				}
				if len(bad) > 0 {
					problem(i, "serial_not_in_stock", fmt.Sprint(bad))
					continue
				}
				if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(serial ORDER BY serial), '{}') FROM stock_serial
					WHERE product_id = $1 AND serial = ANY($2) AND warehouse_id <> $3`, l.GetProductId(), l.GetSerials(),
					l.GetWarehouseId()).Scan(&bad); err != nil {
					return err
				}
				if len(bad) > 0 {
					problem(i, "serial_in_other_warehouse", fmt.Sprint(bad))
					continue
				}
				allocs = append(allocs, allocation{line: i, quantity: qty[i]})
			case p.Tracking == "batch":
				got, err := m.allocateBatches(ctx, tx, i, l, qty[i])
				if err != nil {
					return err
				}
				if got == nil {
					problem(i, "insufficient_batch", "the batch, or the batches together, do not hold enough")
					continue
				}
				allocs = append(allocs, got...)
			default:
				allocs = append(allocs, allocation{line: i, quantity: qty[i]})
			}
		}
		if len(problems) > 0 {
			return &refused{problems}
		}
		var moves []string
		for _, a := range allocs {
			l := in.GetLines()[a.line]
			k := keys[a.line]
			serials := slices.Clone(l.GetSerials())
			if serials == nil {
				serials = []string{}
			}
			slices.Sort(serials)
			id := ids.New().String()
			var lineID *string
			if l.GetSourceLineId() != "" {
				s := l.GetSourceLineId()
				lineID = &s
			}
			if _, err := tx.Exec(ctx, `INSERT INTO movement (id, kind, product_id, warehouse_id, legal_entity_id, quantity,
				batch, serials, business_date, ref_kind, ref_id, ref_line_id, branch_id, consumption_id)
				VALUES ($1, 'issue', $2, $3, $4, $5::numeric, $6, $7, $8::date, $9, $10, $11, $12, $13)`, id, k.product,
				k.warehouse, *b.LegalEntityID, a.quantity, a.batch, serials, in.GetBusinessDate(),
				in.GetReference().GetKind(), in.GetReference().GetId(), lineID, b.ID, in.GetConsumptionId()); err != nil {
				return err
			}
			if a.batch != nil {
				if _, err := tx.Exec(ctx, `UPDATE stock_lot SET on_hand = on_hand - $4::numeric WHERE product_id = $1
					AND warehouse_id = $2 AND batch = $3`, k.product, k.warehouse, *a.batch, a.quantity); err != nil {
					return err
				}
			}
			if len(serials) > 0 {
				if _, err := tx.Exec(ctx, `UPDATE stock_serial SET in_stock = false, updated_at = now() WHERE product_id = $1
					AND serial = ANY($2)`, k.product, serials); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE branch_line SET consumed = consumed + $4::numeric WHERE branch_id = $1
				AND product_id = $2 AND warehouse_id = $3`, b.ID, k.product, k.warehouse, a.quantity); err != nil {
				return err
			}
			// the issue takes what the branch held: on hand and reserved fall together, available stays
			if err := m.moveStock(ctx, tx, k, neg(a.quantity), neg(a.quantity)); err != nil {
				return err
			}
			moves = append(moves, id)
		}
		var left bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM branch_line WHERE branch_id = $1 AND consumed < quantity)`,
			b.ID).Scan(&left); err != nil {
			return err
		}
		state := "committed"
		if !left {
			state = "consumed"
		}
		if b, err = m.setState(ctx, tx, b.ID, state); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE consumption SET movement_ids = $2 WHERE id = $1`, in.GetConsumptionId(), moves); err != nil {
			return err
		}
		if err := m.change(ctx, tx, caller, OpSysIssue, Movements, moves[0], map[string]any{"consumption_id": in.GetConsumptionId(),
			"branch_id": b.ID, "movements": moves}); err != nil {
			return err
		}
		resp = &inventoryv3.IssueResponse{ConsumptionId: in.GetConsumptionId(), Issued: true, MovementIds: moves,
			BranchState: branchStates[b.State]}
		return nil
	})
	var r *refused
	if errors.As(err, &r) {
		return &inventoryv3.IssueResponse{ConsumptionId: in.GetConsumptionId(), Problems: r.problems}, nil
	}
	if err != nil {
		return nil, err
	}
	m.numberNow(ctx)
	return resp, nil
}

// allocateBatches takes a named batch, or the batches oldest receipt first (FIFO, the replaceable allocation policy);
// nil when they do not hold enough. The batches are locked in their key order.
func (m *Module) allocateBatches(ctx context.Context, tx db.Tx, i int, l *inventoryv3.IssueLine, qty string) ([]allocation, error) {
	q := `SELECT batch, trim_scale(on_hand)::text FROM stock_lot WHERE product_id = $1 AND warehouse_id = $2 AND on_hand > 0`
	args := []any{l.GetProductId(), l.GetWarehouseId()}
	if l.GetBatch() != "" {
		q += ` AND batch = $3`
		args = append(args, l.GetBatch())
	}
	rows, err := tx.Query(ctx, q+` ORDER BY first_received_at, batch FOR UPDATE`, args...)
	if err != nil {
		return nil, err
	}
	type lot struct{ batch, onHand string }
	var lots []lot
	for rows.Next() {
		var x lot
		if err := rows.Scan(&x.batch, &x.onHand); err != nil {
			rows.Close()
			return nil, err
		}
		lots = append(lots, x)
	}
	rows.Close()
	var out []allocation
	need := qty
	for _, x := range lots {
		var take, rest string
		if err := tx.QueryRow(ctx, `SELECT trim_scale(least($1::numeric, $2::numeric))::text,
			trim_scale($1::numeric - least($1::numeric, $2::numeric))::text`, need, x.onHand).Scan(&take, &rest); err != nil {
			return nil, err
		}
		b := x.batch
		out = append(out, allocation{line: i, batch: &b, quantity: take})
		need = rest
		if need == "0" {
			return out, nil
		}
	}
	return nil, nil
}

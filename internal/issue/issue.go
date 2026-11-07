package issue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	branches "github.com/brickKit/erp-inventory/v3/internal/branches"
	core "github.com/brickKit/erp-inventory/v3/internal/core"
	masterdata "github.com/brickKit/erp-inventory/v3/internal/masterdata"
	movements "github.com/brickKit/erp-inventory/v3/internal/movements"
	numbering "github.com/brickKit/erp-inventory/v3/internal/numbering"
	stock "github.com/brickKit/erp-inventory/v3/internal/stock"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/grpcx"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/reserve"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	"google.golang.org/protobuf/proto"
)

// refused carries the problems of an issue out of its transaction, which then rolls back: nothing is written.
type refused struct{ problems []*inventoryv3.IssueProblem }

func (r *refused) Error() string {
	return fmt.Sprintf("the issue is refused: %d problems", len(r.problems))
}

// allocation is one movement an issue line becomes: a batch-tracked line may take several batches (FIFO).
type allocation struct {
	line     int
	batch    *string
	quantity string
}

// Issue writes the issue movements and consumes the branch in one transaction (RS19); it is idempotent by
// consumption_id, and the whole issue is refused when any line cannot be issued.
func Issue(m *core.Base, ctx context.Context, in *inventoryv3.IssueRequest) (*inventoryv3.IssueResponse, error) {
	if in.GetConsumptionId() == "" || len(in.GetConsumptionId()) > 128 {
		return nil, core.Invalid("consumption_id", "consumption_id is the coordinator's delivery ID")
	}
	if err := branches.BranchID(in.GetBranchId()); err != nil {
		return nil, err
	}
	if !movements.DateForm.MatchString(in.GetBusinessDate()) {
		return nil, core.Invalid("business_date", "business_date is YYYY-MM-DD")
	}
	if len(in.GetLines()) == 0 || len(in.GetLines()) > core.MaxBatch {
		return nil, core.Invalid("lines", fmt.Sprintf("1-%d lines", core.MaxBatch))
	}
	var pids []string
	qty := make([]string, len(in.GetLines()))
	for i, l := range in.GetLines() {
		if !core.UUIDForm.MatchString(l.GetProductId()) || !core.UUIDForm.MatchString(l.GetWarehouseId()) {
			return nil, core.Invalid(fmt.Sprintf("lines[%d]", i), "product_id and warehouse_id are IDs")
		}
		q, err := core.Quantity(fmt.Sprintf("lines[%d].quantity", i), l.GetQuantity())
		if err != nil {
			return nil, err
		}
		qty[i] = q
		pids = append(pids, l.GetProductId())
	}
	body, _ := proto.MarshalOptions{Deterministic: true}.Marshal(in)
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	products, perr := masterdata.ProductsOf(m, ctx, pids)
	caller, _ := grpcx.Caller(ctx)
	var resp *inventoryv3.IssueResponse
	err := m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		var moves []string
		a, err := reserve.Consume(ctx, tx, branches.Participant(m), in.GetBranchId(), in.GetConsumptionId(), digest,
			func(ctx context.Context, tx db.Tx, rb reserve.Branch) (bool, bool, error) {
				left, err := issueFrom(m, ctx, tx, in, rb, products, perr, qty, caller, &moves)
				return left, false, err
			})
		if err != nil {
			return err
		}
		switch {
		case a.Conflict:
			return core.ErrContent.New("consumption " + in.GetConsumptionId() + " was issued with other content")
		case a.Repeated: // issued before: the same answer
			if err := tx.QueryRow(ctx, `SELECT movement_ids::text[] FROM consumption WHERE id = $1`,
				in.GetConsumptionId()).Scan(&moves); err != nil {
				return err
			}
		case a.Refused != "": // no branch, or not committed (the SDK checks it under the lock)
			state := ""
			if a.Branch.State != reserve.Absent {
				state = string(a.Branch.State)
			}
			return &refused{[]*inventoryv3.IssueProblem{{Line: 0, Reason: "branch_not_committed",
				Detail: "the branch is not committed: " + state}}}
		}
		resp = &inventoryv3.IssueResponse{ConsumptionId: in.GetConsumptionId(), Issued: true, MovementIds: moves,
			BranchState: branches.BranchStates[string(a.Branch.State)]}
		return nil
	})
	var r *refused
	if errors.As(err, &r) {
		return &inventoryv3.IssueResponse{ConsumptionId: in.GetConsumptionId(), Problems: r.problems}, nil
	}
	if err != nil {
		return nil, err
	}
	numbering.NumberNow(m, ctx)
	return resp, nil
}

// issueFrom issues the request from committed branch rb (its positions and row locked by the SDK): the lines within
// what the branch has left, the batches and serial numbers its products track; it records the movements, consumes
// the branch's lines, keeps the consumption's movements (moves) and audits the issue. A request with problems is
// refused whole (refused). It answers whether something is left on the branch.
func issueFrom(m *core.Base, ctx context.Context, tx db.Tx, in *inventoryv3.IssueRequest, rb reserve.Branch,
	products map[string]masterdata.Product, perr error, qty []string, caller string, out *[]string) (bool, error) {
	if perr != nil {
		return false, perr
	}
	var problems []*inventoryv3.IssueProblem
	problem := func(i int, reason, detail string) {
		problems = append(problems, &inventoryv3.IssueProblem{Line: int32(i), Reason: reason, Detail: detail})
	}
	b, err := core.GetBranch(ctx, tx, rb.ID)
	if err != nil {
		return false, err
	}
	// the branch's remaining quantity per product and warehouse, less what earlier lines of this issue take
	remaining := map[stock.Key]string{}
	for _, l := range b.Lines {
		var r string
		if err := tx.QueryRow(ctx, `SELECT trim_scale($1::numeric - $2::numeric)::text`, l.Quantity, l.Consumed).
			Scan(&r); err != nil {
			return false, err
		}
		remaining[stock.Key{Product: l.ProductID, Warehouse: l.WarehouseID}] = r
	}
	keys := make([]stock.Key, len(in.GetLines()))
	for i, l := range in.GetLines() {
		keys[i] = stock.Key{Product: l.GetProductId(), Warehouse: l.GetWarehouseId()}
	}
	if _, err := stock.LockPositions(ctx, tx, *b.LegalEntityID, keys); err != nil {
		return false, err
	}
	var allocs []allocation
	for i, l := range in.GetLines() {
		k := keys[i]
		rest, ok := remaining[k]
		enough := false
		if ok {
			if enough, err = stock.AtLeast(ctx, tx, rest, qty[i]); err != nil {
				return false, err
			}
		}
		if !enough {
			problem(i, "exceeds_branch", "more than the branch has left for this product and warehouse")
			continue
		}
		if err := tx.QueryRow(ctx, `SELECT trim_scale($1::numeric - $2::numeric)::text`, rest, qty[i]).
			Scan(&rest); err != nil {
			return false, err
		}
		remaining[k] = rest
		p := products[l.GetProductId()]
		if err := movements.CheckTracking("", p, qty[i], l.GetBatch(), l.GetSerials(), false); err != nil {
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
				return false, err
			}
			if len(bad) > 0 {
				problem(i, "serial_not_found", fmt.Sprint(bad))
				continue
			}
			if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(serial ORDER BY serial), '{}') FROM stock_serial
					WHERE product_id = $1 AND serial = ANY($2) AND NOT in_stock`, l.GetProductId(), l.GetSerials()).
				Scan(&bad); err != nil {
				return false, err
			}
			if len(bad) > 0 {
				problem(i, "serial_not_in_stock", fmt.Sprint(bad))
				continue
			}
			if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(serial ORDER BY serial), '{}') FROM stock_serial
					WHERE product_id = $1 AND serial = ANY($2) AND warehouse_id <> $3`, l.GetProductId(), l.GetSerials(),
				l.GetWarehouseId()).Scan(&bad); err != nil {
				return false, err
			}
			if len(bad) > 0 {
				problem(i, "serial_in_other_warehouse", fmt.Sprint(bad))
				continue
			}
			allocs = append(allocs, allocation{line: i, quantity: qty[i]})
		case p.Tracking == "batch":
			got, err := allocateBatches(m, ctx, tx, i, l, qty[i])
			if err != nil {
				return false, err
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
		return false, &refused{problems}
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
				VALUES ($1, 'issue', $2, $3, $4, $5::numeric, $6, $7, $8::date, $9, $10, $11, $12, $13)`, id, k.Product,
			k.Warehouse, *b.LegalEntityID, a.quantity, a.batch, serials, in.GetBusinessDate(),
			in.GetReference().GetKind(), in.GetReference().GetId(), lineID, b.ID, in.GetConsumptionId()); err != nil {
			return false, err
		}
		if a.batch != nil {
			if _, err := tx.Exec(ctx, `UPDATE stock_lot SET on_hand = on_hand - $4::numeric WHERE product_id = $1
					AND warehouse_id = $2 AND batch = $3`, k.Product, k.Warehouse, *a.batch, a.quantity); err != nil {
				return false, err
			}
		}
		if len(serials) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE stock_serial SET in_stock = false, updated_at = now() WHERE product_id = $1
					AND serial = ANY($2)`, k.Product, serials); err != nil {
				return false, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE branch_line SET consumed = consumed + $4::numeric WHERE branch_id = $1
				AND product_id = $2 AND warehouse_id = $3`, b.ID, k.Product, k.Warehouse, a.quantity); err != nil {
			return false, err
		}
		// the issue takes what the branch held: on hand and reserved fall together, available stays
		if err := stock.MoveStock(m, ctx, tx, k, stock.Neg(a.quantity), stock.Neg(a.quantity)); err != nil {
			return false, err
		}
		moves = append(moves, id)
	}
	var left bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM branch_line WHERE branch_id = $1 AND consumed < quantity)`,
		b.ID).Scan(&left); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO consumption (id, branch_id, content_digest, movement_ids) VALUES ($1, $2, $3, $4)`,
		in.GetConsumptionId(), b.ID, rb.Digest, moves); err != nil {
		return false, err
	}
	*out = moves
	return left, core.Change(m, ctx, tx, caller, core.OpSysIssue, core.Movements, moves[0], map[string]any{
		"consumption_id": in.GetConsumptionId(), "branch_id": b.ID, "movements": moves})
}

// allocateBatches takes a named batch, or the batches oldest receipt first (FIFO by the system time a batch number was
// first received, the replaceable allocation policy); nil when they do not hold enough. The batch rows are locked in
// that order, under the position's lock (lockPositions), so two issues never wait on each other's batches.
func allocateBatches(m *core.Base, ctx context.Context, tx db.Tx, i int, l *inventoryv3.IssueLine, qty string) ([]allocation, error) {
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

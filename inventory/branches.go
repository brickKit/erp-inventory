package inventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/errs"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/work"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
)

// The branch states of protocol 04 and their wire values.
var branchStates = map[string]inventoryv3.BranchState{
	"held": inventoryv3.BranchState_BRANCH_STATE_HELD, "committed": inventoryv3.BranchState_BRANCH_STATE_COMMITTED,
	"consumed": inventoryv3.BranchState_BRANCH_STATE_CONSUMED, "released": inventoryv3.BranchState_BRANCH_STATE_RELEASED,
	"expired": inventoryv3.BranchState_BRANCH_STATE_EXPIRED, "rejected": inventoryv3.BranchState_BRANCH_STATE_REJECTED,
	"cancelled": inventoryv3.BranchState_BRANCH_STATE_CANCELLED,
}

func wire(b Branch) *inventoryv3.Branch {
	out := &inventoryv3.Branch{BranchId: b.ID, State: branchStates[b.State], Version: b.Version}
	if b.Reason != nil {
		out.Reason = *b.Reason
	}
	if b.HoldExpiresAt != nil {
		out.HoldExpiresAt = *b.HoldExpiresAt
	}
	for _, l := range b.Lines {
		out.Lines = append(out.Lines, &inventoryv3.BranchLine{ProductId: l.ProductID, WarehouseId: l.WarehouseID,
			Quantity: l.Quantity, Consumed: l.Consumed})
	}
	for _, s := range b.Shortages {
		out.Shortages = append(out.Shortages, &inventoryv3.Shortage{ProductId: s.ProductID, WarehouseId: s.WarehouseID,
			Requested: s.Requested, Available: s.Available, Reason: s.Reason})
	}
	return out
}

func absent(id string) *inventoryv3.Branch {
	return &inventoryv3.Branch{BranchId: id, State: inventoryv3.BranchState_BRANCH_STATE_ABSENT}
}

func branchID(id string) error {
	if !uuidForm.MatchString(id) {
		return invalid("branch_id", "branch_id is the coordinator's UUID for the branch")
	}
	return nil
}

// holdContent is what makes two holds of one branch ID the same: legal entity, reference and lines (RS17).
func holdContent(in *inventoryv3.HoldRequest, lines []BranchLine) string {
	b, _ := json.Marshal(map[string]any{"le": in.GetLegalEntityId(), "ref": in.GetReference().GetKind() + "/" +
		in.GetReference().GetId() + "/" + fmt.Sprint(in.GetReference().GetRevision()), "lines": lines})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Hold reserves all lines of a branch or none (protocol 04; docs/rules/availability.md).
func (m *Module) Hold(ctx context.Context, in *inventoryv3.HoldRequest) (*inventoryv3.HoldResponse, error) {
	if err := branchID(in.GetBranchId()); err != nil {
		return nil, err
	}
	if !uuidForm.MatchString(in.GetLegalEntityId()) {
		return nil, invalid("legal_entity_id", "legal_entity_id is a legal entity's ID")
	}
	if len(in.GetLines()) == 0 || len(in.GetLines()) > MaxBatch {
		return nil, invalid("lines", fmt.Sprintf("1-%d lines", MaxBatch))
	}
	var lines []BranchLine
	seen := map[key]bool{}
	var pids []string
	for i, l := range in.GetLines() {
		f := fmt.Sprintf("lines[%d]", i)
		if !uuidForm.MatchString(l.GetProductId()) || !uuidForm.MatchString(l.GetWarehouseId()) {
			return nil, invalid(f, "product_id and warehouse_id are IDs")
		}
		k := key{l.GetProductId(), l.GetWarehouseId()}
		if seen[k] {
			return nil, invalid(f, "one line per product and warehouse")
		}
		seen[k] = true
		q, err := quantity(f+".quantity", l.GetQuantity())
		if err != nil {
			return nil, err
		}
		lines = append(lines, BranchLine{ProductID: k.product, WarehouseID: k.warehouse, Quantity: q, Consumed: "0"})
		pids = append(pids, k.product)
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].ProductID != lines[j].ProductID {
			return lines[i].ProductID < lines[j].ProductID
		}
		return lines[i].WarehouseID < lines[j].WarehouseID
	})
	digest := holdContent(in, lines)
	// The products before the transaction (no call inside one); a hold seen before answers without them.
	products, perr := m.productsOf(ctx, pids)
	var out Branch
	err := m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO branch (id, state, content_digest) VALUES ($1, 'held', $2)
			ON CONFLICT (id) DO NOTHING`, in.GetBranchId(), digest)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 { // seen before: a tombstone, the same hold again, or a conflict
			b, err := getBranch(ctx, tx, in.GetBranchId(), true)
			if err != nil {
				return err
			}
			if b.State != "cancelled" && (b.digest == nil || *b.digest != digest) {
				return errContent.New("branch " + b.ID + " was held with other content")
			}
			out = b
			return nil
		}
		if perr != nil {
			return perr
		}
		var shortages []Shortage
		short := func(l BranchLine, avail, reason string) {
			shortages = append(shortages, Shortage{ProductID: l.ProductID, WarehouseID: l.WarehouseID,
				Requested: l.Quantity, Available: avail, Reason: reason})
		}
		for _, l := range lines {
			if p, ok := products[l.ProductID]; !ok || p.Kind != "stock" {
				short(l, "0", "product_not_stocked")
			}
		}
		for _, l := range lines {
			if _, err := usableWarehouses(ctx, tx, in.GetLegalEntityId(), []string{l.WarehouseID}); err != nil {
				reason := "warehouse_not_found"
				if e := errs.From(err); e != nil && e.Reason == "WAREHOUSE_NOT_USABLE" {
					w, gerr := getWarehouse(ctx, tx, l.WarehouseID, false)
					switch {
					case errors.Is(gerr, errNone):
					case gerr != nil:
						return gerr
					case w.LegalEntityID != in.GetLegalEntityId():
						reason = "warehouse_not_in_legal_entity"
					default:
						reason = "warehouse_inactive"
					}
				}
				short(l, "0", reason)
			}
		}
		state, reason := "held", ""
		if len(shortages) > 0 {
			state, reason = "rejected", "invalid_line"
		} else {
			keys := make([]key, len(lines))
			for i, l := range lines {
				keys[i] = key{l.ProductID, l.WarehouseID}
			}
			pos, err := lockPositions(ctx, tx, in.GetLegalEntityId(), keys)
			if err != nil {
				return err
			}
			for i, l := range lines {
				ok, err := atLeast(ctx, tx, pos[keys[i]].Available, l.Quantity)
				if err != nil {
					return err
				}
				if !ok {
					short(l, pos[keys[i]].Available, "insufficient")
				}
			}
			if len(shortages) > 0 {
				state, reason = "rejected", "insufficient_stock"
			} else {
				for i, l := range lines {
					if err := m.moveStock(ctx, tx, keys[i], "0", l.Quantity); err != nil {
						return err
					}
				}
			}
		}
		if shortages == nil {
			shortages = []Shortage{}
		}
		sj, _ := json.Marshal(shortages)
		var reasonCol *string
		if reason != "" {
			reasonCol = &reason
		}
		ref := in.GetReference()
		if _, err := tx.Exec(ctx, `UPDATE branch SET legal_entity_id = $2, ref_kind = $3, ref_id = $4, ref_revision = $5,
			state = $6, reason = $7, shortages = $8, hold_expires_at = CASE WHEN $6 = 'held' THEN now() + $9::interval END
			WHERE id = $1`, in.GetBranchId(), in.GetLegalEntityId(), ref.GetKind(), ref.GetId(), ref.GetRevision(), state,
			reasonCol, sj, fmt.Sprintf("%d seconds", int(HoldTTL.Seconds()))); err != nil {
			return err
		}
		for _, l := range lines {
			if _, err := tx.Exec(ctx, `INSERT INTO branch_line (branch_id, product_id, warehouse_id, quantity)
				VALUES ($1, $2, $3, $4::numeric)`, in.GetBranchId(), l.ProductID, l.WarehouseID, l.Quantity); err != nil {
				return err
			}
		}
		if out, err = getBranch(ctx, tx, in.GetBranchId(), false); err != nil {
			return err
		}
		return m.emit(ctx, tx, BranchChanged, out.ID, out.Version, out)
	})
	if err != nil {
		return nil, err
	}
	return &inventoryv3.HoldResponse{Branch: wire(out)}, nil
}

// release gives back what a held or committed branch has not consumed and moves it to state (released or expired).
func (m *Module) release(ctx context.Context, tx db.Tx, b Branch, state string) (Branch, error) {
	var keys []key
	for _, l := range b.Lines {
		keys = append(keys, key{l.ProductID, l.WarehouseID})
	}
	if _, err := lockPositions(ctx, tx, *b.LegalEntityID, keys); err != nil {
		return b, err
	}
	for _, l := range b.Lines {
		var rest string
		if err := tx.QueryRow(ctx, `SELECT trim_scale($1::numeric - $2::numeric)::text`, l.Quantity, l.Consumed).
			Scan(&rest); err != nil {
			return b, err
		}
		if rest != "0" {
			if err := m.moveStock(ctx, tx, key{l.ProductID, l.WarehouseID}, "0", neg(rest)); err != nil {
				return b, err
			}
		}
	}
	return m.setState(ctx, tx, b.ID, state)
}

func (m *Module) setState(ctx context.Context, tx db.Tx, id, state string) (Branch, error) {
	if _, err := tx.Exec(ctx, `UPDATE branch SET state = $2, hold_expires_at = CASE WHEN $2 = 'held' THEN hold_expires_at END,
		confirmed_at = CASE WHEN $2 = 'committed' THEN now() ELSE confirmed_at END, version = version + 1,
		updated_at = now() WHERE id = $1`, id, state); err != nil {
		return Branch{}, err
	}
	b, err := getBranch(ctx, tx, id, false)
	if err != nil {
		return b, err
	}
	return b, m.emit(ctx, tx, BranchChanged, b.ID, b.Version, b)
}

// expired reports whether a held branch is past its hold period, by the database clock.
func expired(ctx context.Context, tx db.Tx, id string) (bool, error) {
	var past bool
	err := tx.QueryRow(ctx, `SELECT hold_expires_at <= now() FROM branch WHERE id = $1`, id).Scan(&past)
	return past, err
}

// Confirm turns a held branch into a committed one; a held branch past its period expires here instead (the same
// row lock as the expiry job, so the two never both win), and an expired one is never revived.
func (m *Module) Confirm(ctx context.Context, in *inventoryv3.ConfirmRequest) (*inventoryv3.ConfirmResponse, error) {
	b, err := m.onBranch(ctx, in.GetBranchId(), func(ctx context.Context, tx db.Tx, b Branch) (Branch, error) {
		if b.State != "held" {
			return b, nil
		}
		past, err := expired(ctx, tx, b.ID)
		if err != nil {
			return b, err
		}
		if past {
			return m.release(ctx, tx, b, "expired")
		}
		return m.setState(ctx, tx, b.ID, "committed")
	})
	if err != nil {
		return nil, err
	}
	return &inventoryv3.ConfirmResponse{Branch: b}, nil
}

// Cancel releases what the branch has not consumed; an unknown branch becomes a tombstone that refuses a late hold.
func (m *Module) Cancel(ctx context.Context, in *inventoryv3.CancelRequest) (*inventoryv3.CancelResponse, error) {
	if err := branchID(in.GetBranchId()); err != nil {
		return nil, err
	}
	var out Branch
	err := m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO branch (id, state) VALUES ($1, 'cancelled') ON CONFLICT (id) DO NOTHING`,
			in.GetBranchId())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			if out, err = getBranch(ctx, tx, in.GetBranchId(), false); err != nil {
				return err
			}
			return m.emit(ctx, tx, BranchChanged, out.ID, out.Version, out)
		}
		b, err := getBranch(ctx, tx, in.GetBranchId(), true)
		if err != nil {
			return err
		}
		out = b
		if b.State == "held" || b.State == "committed" {
			out, err = m.release(ctx, tx, b, "released")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &inventoryv3.CancelResponse{Branch: wire(out)}, nil
}

// Query answers the branch as it is; it leaves no tombstone.
func (m *Module) Query(ctx context.Context, in *inventoryv3.QueryRequest) (*inventoryv3.QueryResponse, error) {
	b, err := m.onBranch(ctx, in.GetBranchId(), func(_ context.Context, _ db.Tx, b Branch) (Branch, error) { return b, nil })
	if err != nil {
		return nil, err
	}
	return &inventoryv3.QueryResponse{Branch: b}, nil
}

// Adjust applies an order revision (protocol 04): not in this version.
func (m *Module) Adjust(context.Context, *inventoryv3.AdjustRequest) (*inventoryv3.AdjustResponse, error) {
	return nil, errs.New(errs.ReasonCapabilityUnsupported, "branch adjustment (order revisions) is not in this version of erp/inventory")
}

// onBranch locks an existing branch and applies fn; an unknown branch answers ABSENT.
func (m *Module) onBranch(ctx context.Context, id string, fn func(ctx context.Context, tx db.Tx, b Branch) (Branch, error)) (*inventoryv3.Branch, error) {
	if err := branchID(id); err != nil {
		return nil, err
	}
	var out Branch
	err := m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		b, err := getBranch(ctx, tx, id, true)
		if err != nil {
			return err
		}
		out, err = fn(ctx, tx, b)
		return err
	})
	if errors.Is(err, errNone) {
		return absent(id), nil
	}
	if err != nil {
		return nil, err
	}
	return wire(out), nil
}

// expireHolds releases held branches past their period, each in its own transaction; the row lock decides against a
// concurrent confirm (whichever locks first wins, the other sees the result).
func (m *Module) expireHolds(ctx context.Context, _ *work.Run) error {
	for {
		var due []string
		err := m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
			due = due[:0]
			rows, err := tx.Query(ctx, `SELECT id::text FROM branch WHERE state = 'held' AND hold_expires_at <= now()
				ORDER BY hold_expires_at LIMIT 100`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				due = append(due, id)
			}
			return rows.Err()
		})
		if err != nil {
			return err
		}
		// each in its own transaction, branch row then positions (RS16): one transaction for the batch would hold the
		// positions of every branch released so far, out of the global order a concurrent hold or issue keeps
		for _, id := range due {
			if err := m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
				b, err := getBranch(ctx, tx, id, true)
				if err != nil || b.State != "held" {
					return err
				}
				var past bool
				if err := tx.QueryRow(ctx, `SELECT hold_expires_at <= now() FROM branch WHERE id = $1::uuid`, id).
					Scan(&past); err != nil || !past {
					return err
				}
				_, err = m.release(ctx, tx, b, "expired")
				return err
			}); err != nil {
				return err
			}
		}
		if len(due) < 100 {
			return nil
		}
	}
}

// getBranchHTTP shows a branch to a user who may read stock in every warehouse of it; a tombstone is not shown.
func (m *Module) getBranchHTTP(c *httpx.Context) error {
	id := c.Param("id")
	if !uuidForm.MatchString(id) {
		return notFound("branch")
	}
	var b Branch
	err := m.rt.DB.Tx(c.Ctx(), func(ctx context.Context, tx db.Tx) error {
		var err error
		b, err = getBranch(ctx, tx, id, false)
		return err
	})
	if errors.Is(err, errNone) || err == nil && b.LegalEntityID == nil {
		return notFound("branch")
	}
	if err != nil {
		return err
	}
	s, _ := sdkauthz.From(c.Ctx())
	for _, l := range b.Lines {
		if _, _, err := s.Check(c.Ctx(), OpStockRead, scope(*b.LegalEntityID, l.WarehouseID)); err != nil {
			return err
		}
	}
	return c.JSON(200, b)
}

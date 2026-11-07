package branches

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	core "github.com/brickKit/erp-inventory/v3/internal/core"
	masterdata "github.com/brickKit/erp-inventory/v3/internal/masterdata"
	stock "github.com/brickKit/erp-inventory/v3/internal/stock"
	warehouses "github.com/brickKit/erp-inventory/v3/internal/warehouses"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/errs"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/reserve"
	"github.com/brickKit/be-sdk-go/v3/work"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
)

// The branch states of protocol 04 and their wire values.
var BranchStates = map[string]inventoryv3.BranchState{
	"held": inventoryv3.BranchState_BRANCH_STATE_HELD, "committed": inventoryv3.BranchState_BRANCH_STATE_COMMITTED,
	"consumed": inventoryv3.BranchState_BRANCH_STATE_CONSUMED, "released": inventoryv3.BranchState_BRANCH_STATE_RELEASED,
	"expired": inventoryv3.BranchState_BRANCH_STATE_EXPIRED, "rejected": inventoryv3.BranchState_BRANCH_STATE_REJECTED,
	"cancelled": inventoryv3.BranchState_BRANCH_STATE_CANCELLED,
}

func wire(b core.Branch) *inventoryv3.Branch {
	out := &inventoryv3.Branch{BranchId: b.ID, State: BranchStates[b.State], Version: b.Version}
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

func BranchID(id string) error {
	if !core.UUIDForm.MatchString(id) {
		return core.Invalid("branch_id", "branch_id is the coordinator's UUID for the branch")
	}
	return nil
}

// holdContent is what makes two holds of one branch ID the same: legal entity, reference and lines (RS17).
func holdContent(in *inventoryv3.HoldRequest, lines []core.BranchLine) string {
	b, _ := json.Marshal(map[string]any{"le": in.GetLegalEntityId(), "ref": in.GetReference().GetKind() + "/" +
		in.GetReference().GetId() + "/" + fmt.Sprint(in.GetReference().GetRevision()), "lines": lines})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Hold reserves all lines of a branch or none (protocol 04; docs/rules/availability.md).
func Hold(m *core.Base, ctx context.Context, in *inventoryv3.HoldRequest) (*inventoryv3.HoldResponse, error) {
	if err := BranchID(in.GetBranchId()); err != nil {
		return nil, err
	}
	if !core.UUIDForm.MatchString(in.GetLegalEntityId()) {
		return nil, core.Invalid("legal_entity_id", "legal_entity_id is a legal entity's ID")
	}
	if len(in.GetLines()) == 0 || len(in.GetLines()) > core.MaxBatch {
		return nil, core.Invalid("lines", fmt.Sprintf("1-%d lines", core.MaxBatch))
	}
	var lines []core.BranchLine
	seen := map[stock.Key]bool{}
	var pids []string
	for i, l := range in.GetLines() {
		f := fmt.Sprintf("lines[%d]", i)
		if !core.UUIDForm.MatchString(l.GetProductId()) || !core.UUIDForm.MatchString(l.GetWarehouseId()) {
			return nil, core.Invalid(f, "product_id and warehouse_id are IDs")
		}
		k := stock.Key{Product: l.GetProductId(), Warehouse: l.GetWarehouseId()}
		if seen[k] {
			return nil, core.Invalid(f, "one line per product and warehouse")
		}
		seen[k] = true
		q, err := core.Quantity(f+".quantity", l.GetQuantity())
		if err != nil {
			return nil, err
		}
		lines = append(lines, core.BranchLine{ProductID: k.Product, WarehouseID: k.Warehouse, Quantity: q, Consumed: "0"})
		pids = append(pids, k.Product)
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].ProductID != lines[j].ProductID {
			return lines[i].ProductID < lines[j].ProductID
		}
		return lines[i].WarehouseID < lines[j].WarehouseID
	})
	digest := holdContent(in, lines)
	// The products before the transaction (no call inside one); a hold seen before answers without them.
	products, perr := masterdata.ProductsOf(m, ctx, pids)
	var out *inventoryv3.Branch
	err := m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		a, err := reserve.Hold(ctx, tx, kind(m), in.GetBranchId(), digest, func(ctx context.Context, tx db.Tx) (bool, string, error) {
			if perr != nil {
				return false, "", perr
			}
			var shortages []core.Shortage
			short := func(l core.BranchLine, avail, reason string) {
				shortages = append(shortages, core.Shortage{ProductID: l.ProductID, WarehouseID: l.WarehouseID,
					Requested: l.Quantity, Available: avail, Reason: reason})
			}
			for _, l := range lines {
				if p, ok := products[l.ProductID]; !ok || p.Kind != "stock" {
					short(l, "0", "product_not_stocked")
				}
			}
			for _, l := range lines {
				if _, err := warehouses.UsableWarehouses(ctx, tx, in.GetLegalEntityId(), []string{l.WarehouseID}); err != nil {
					reason := "warehouse_not_found"
					if e := errs.From(err); e != nil && e.Reason == "WAREHOUSE_NOT_USABLE" {
						w, gerr := core.GetWarehouse(ctx, tx, l.WarehouseID, false)
						switch {
						case errors.Is(gerr, core.ErrNone):
						case gerr != nil:
							return false, "", gerr
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
				keys := make([]stock.Key, len(lines))
				for i, l := range lines {
					keys[i] = stock.Key{Product: l.ProductID, Warehouse: l.WarehouseID}
				}
				pos, err := stock.LockPositions(ctx, tx, in.GetLegalEntityId(), keys)
				if err != nil {
					return false, "", err
				}
				for i, l := range lines {
					ok, err := stock.AtLeast(ctx, tx, pos[keys[i]].Available, l.Quantity)
					if err != nil {
						return false, "", err
					}
					if !ok {
						short(l, pos[keys[i]].Available, "insufficient")
					}
				}
				if len(shortages) > 0 {
					state, reason = "rejected", "insufficient_stock"
				} else {
					for i, l := range lines {
						if err := stock.MoveStock(m, ctx, tx, keys[i], "0", l.Quantity); err != nil {
							return false, "", err
						}
					}
				}
			}
			if shortages == nil {
				shortages = []core.Shortage{}
			}
			sj, _ := json.Marshal(shortages)
			ref := in.GetReference()
			if _, err := tx.Exec(ctx, `INSERT INTO branch (id, legal_entity_id, ref_kind, ref_id, ref_revision, shortages)
			VALUES ($1, $2, $3, $4, $5, $6)`, in.GetBranchId(), in.GetLegalEntityId(), ref.GetKind(), ref.GetId(),
				ref.GetRevision(), sj); err != nil {
				return false, "", err
			}
			for _, l := range lines {
				if _, err := tx.Exec(ctx, `INSERT INTO branch_line (branch_id, product_id, warehouse_id, quantity)
				VALUES ($1, $2, $3, $4::numeric)`, in.GetBranchId(), l.ProductID, l.WarehouseID, l.Quantity); err != nil {
					return false, "", err
				}
			}
			return state == "held", reason, nil
		})
		if err != nil {
			return err
		}
		if a.Conflict {
			return core.ErrContent.New("branch " + in.GetBranchId() + " was held with other content")
		}
		out, err = answered(ctx, tx, a)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &inventoryv3.HoldResponse{Branch: out}, nil
}

// Kind names this participation's branches in the SDK's table.
const Kind = "erp.inventory.order"

// kind is erp/inventory's participation in protocol 04 (be-protocol chapter 16): the branch's state is the SDK's
// (reserve), what it takes is stock reserved on its lines' positions.
func kind(m *core.Base) reserve.Kind {
	return reserve.Kind{Name: Kind, Period: core.HoldTTL,
		Lock: func(ctx context.Context, tx db.Tx, id string) error { // the lines' positions, in order, before the branch
			b, err := core.GetBranch(ctx, tx, id)
			if err != nil || b.LegalEntityID == nil || len(b.Lines) == 0 {
				return err // a tombstone locks nothing
			}
			_, err = stock.LockPositions(ctx, tx, *b.LegalEntityID, keysOf(b))
			return err
		},
		GiveBack: func(ctx context.Context, tx db.Tx, rb reserve.Branch) error {
			b, err := core.GetBranch(ctx, tx, rb.ID)
			if err != nil {
				return err
			}
			for _, l := range b.Lines {
				var rest string
				if err := tx.QueryRow(ctx, `SELECT trim_scale($1::numeric - $2::numeric)::text`, l.Quantity, l.Consumed).
					Scan(&rest); err != nil {
					return err
				}
				if rest != "0" {
					if err := stock.MoveStock(m, ctx, tx, stock.Key{Product: l.ProductID, Warehouse: l.WarehouseID}, "0",
						stock.Neg(rest)); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Changed: func(ctx context.Context, tx db.Tx, rb reserve.Branch) error {
			b, err := core.GetBranch(ctx, tx, rb.ID)
			if err != nil {
				return err
			}
			return core.Emit(m, ctx, tx, core.BranchChanged, b.ID, b.Version, b)
		}}
}

// Participant is this participation for the issue, which consumes branches in its own transaction.
func Participant(m *core.Base) reserve.Kind { return kind(m) }

func keysOf(b core.Branch) []stock.Key {
	keys := make([]stock.Key, len(b.Lines))
	for i, l := range b.Lines {
		keys[i] = stock.Key{Product: l.ProductID, Warehouse: l.WarehouseID}
	}
	return keys
}

// answered is the branch after a command, read in the command's transaction; Absent answers ABSENT.
func answered(ctx context.Context, tx db.Tx, a reserve.Answer) (*inventoryv3.Branch, error) {
	if a.Branch.State == reserve.Absent {
		return absent(a.Branch.ID), nil
	}
	b, err := core.GetBranch(ctx, tx, a.Branch.ID)
	if err != nil {
		return nil, err
	}
	return wire(b), nil
}

// onBranch runs a command on a branch in a transaction and answers it (ABSENT for none stored).
func onBranch(m *core.Base, ctx context.Context, id string, command func(ctx context.Context, tx db.Tx) (reserve.Answer, error)) (*inventoryv3.Branch, error) {
	if err := BranchID(id); err != nil {
		return nil, err
	}
	var out *inventoryv3.Branch
	err := m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		a, err := command(ctx, tx)
		if err != nil {
			return err
		}
		out, err = answered(ctx, tx, a)
		return err
	})
	return out, err
}

// Confirm turns a held branch into a committed one; a held branch past its period expires here instead (the same
// lock as the expiry job, so the two never both win), and an expired one is never revived.
func Confirm(m *core.Base, ctx context.Context, in *inventoryv3.ConfirmRequest) (*inventoryv3.ConfirmResponse, error) {
	b, err := onBranch(m, ctx, in.GetBranchId(), func(ctx context.Context, tx db.Tx) (reserve.Answer, error) {
		return reserve.Confirm(ctx, tx, kind(m), in.GetBranchId())
	})
	if err != nil {
		return nil, err
	}
	return &inventoryv3.ConfirmResponse{Branch: b}, nil
}

// Cancel releases what the branch has not consumed; an unknown branch becomes a tombstone that refuses a late hold.
func Cancel(m *core.Base, ctx context.Context, in *inventoryv3.CancelRequest) (*inventoryv3.CancelResponse, error) {
	b, err := onBranch(m, ctx, in.GetBranchId(), func(ctx context.Context, tx db.Tx) (reserve.Answer, error) {
		return reserve.Cancel(ctx, tx, kind(m), in.GetBranchId())
	})
	if err != nil {
		return nil, err
	}
	return &inventoryv3.CancelResponse{Branch: b}, nil
}

// Query answers the branch as it is; it leaves no tombstone.
func Query(m *core.Base, ctx context.Context, in *inventoryv3.QueryRequest) (*inventoryv3.QueryResponse, error) {
	b, err := onBranch(m, ctx, in.GetBranchId(), func(ctx context.Context, tx db.Tx) (reserve.Answer, error) {
		return reserve.Query(ctx, tx, kind(m), in.GetBranchId())
	})
	if err != nil {
		return nil, err
	}
	return &inventoryv3.QueryResponse{Branch: b}, nil
}

// Adjust applies an order revision (protocol 04): not in this version.
func Adjust(m *core.Base, _ context.Context, _ *inventoryv3.AdjustRequest) (*inventoryv3.AdjustResponse, error) {
	return nil, errs.New(errs.ReasonCapabilityUnsupported, "branch adjustment (order revisions) is not in this version of erp/inventory")
}

// ExpireHolds releases held branches past their period (the SDK's expiry: each in its own transaction, the
// positions then the branch).
func ExpireHolds(m *core.Base, ctx context.Context, _ *work.Run) error {
	return reserve.ExpireDue(ctx, m.RT.DB, kind(m))
}

// GetBranchHTTP shows a branch to a user who may read stock in every warehouse of it; a tombstone is not shown.
func GetBranchHTTP(m *core.Base, c *httpx.Context) error {
	id := c.Param("id")
	if !core.UUIDForm.MatchString(id) {
		return core.NotFound("branch")
	}
	var b core.Branch
	err := m.RT.DB.Tx(c.Ctx(), func(ctx context.Context, tx db.Tx) error {
		var err error
		b, err = core.GetBranch(ctx, tx, id)
		return err
	})
	if errors.Is(err, core.ErrNone) || err == nil && b.LegalEntityID == nil {
		return core.NotFound("branch")
	}
	if err != nil {
		return err
	}
	s, _ := sdkauthz.From(c.Ctx())
	for _, l := range b.Lines {
		if _, _, err := s.Check(c.Ctx(), core.OpStockRead, core.Scope(*b.LegalEntityID, l.WarehouseID)); err != nil {
			return err
		}
	}
	return c.JSON(200, b)
}

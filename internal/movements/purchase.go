package movements

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	core "github.com/brickKit/erp-inventory/v3/internal/core"
	masterdata "github.com/brickKit/erp-inventory/v3/internal/masterdata"
	numbering "github.com/brickKit/erp-inventory/v3/internal/numbering"
	stock "github.com/brickKit/erp-inventory/v3/internal/stock"
	warehouses "github.com/brickKit/erp-inventory/v3/internal/warehouses"

	"github.com/brickKit/be-sdk-go/v3/bizdate"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/grpcx"
	"github.com/brickKit/be-sdk-go/v3/ids"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// The purchase component's receipts and returns (3.0.37, StockService.Receive and Return; design.md's rules): a
// receipt is the fact the purchase takes its received quantity from; a return issues from the receipt movement it
// names, its warehouse and batch, at most what it received and has not returned. Each call is kept by its own ID with
// the digest of its content: the same ID and content answers what it wrote, other content is refused.

func digestOf(m proto.Message) string {
	body, _ := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func callID(field, id string) error {
	if id == "" || len(id) > 128 {
		return core.Invalid(field, field+" is 1-128 characters")
	}
	return nil
}

func reference(r *inventoryv3.Reference, kind string) error {
	if r.GetKind() != kind || r.GetId() == "" || len(r.GetId()) > 128 {
		return core.Invalid("reference", "the reference is of kind "+kind+" with the document's ID")
	}
	return nil
}

// dateOf is the business date asked, or the legal entity's today (outside any transaction).
func dateOf(m *core.Base, ctx context.Context, legalEntity, date string) (string, error) {
	if date != "" {
		if !DateForm.MatchString(date) {
			return "", core.Invalid("business_date", "business_date is YYYY-MM-DD")
		}
		return date, nil
	}
	le, err := masterdata.LegalEntityOf(m, ctx, legalEntity)
	if err != nil {
		return "", err
	}
	return bizdate.Today(le.Timezone)
}

// ReceivePurchase is StockService.Receive.
func ReceivePurchase(m *core.Base, ctx context.Context, in *inventoryv3.ReceiveRequest) (*inventoryv3.ReceiveResponse, error) {
	if err := callID("receipt_id", in.GetReceiptId()); err != nil {
		return nil, err
	}
	if !core.UUIDForm.MatchString(in.GetLegalEntityId()) || !core.UUIDForm.MatchString(in.GetWarehouseId()) {
		return nil, core.Invalid("warehouse_id", "legal_entity_id and warehouse_id are IDs")
	}
	if err := reference(in.GetReference(), "purchase_receipt"); err != nil {
		return nil, err
	}
	if len(in.GetLines()) == 0 || len(in.GetLines()) > core.MaxBatch {
		return nil, core.Invalid("lines", fmt.Sprintf("1-%d lines", core.MaxBatch))
	}
	lines := make([]receiptLine, len(in.GetLines()))
	lineIDs := make([]string, len(in.GetLines()))
	var pids []string
	for i, l := range in.GetLines() {
		field := fmt.Sprintf("lines[%d]", i)
		if err := callID(field+".line_id", l.GetLineId()); err != nil {
			return nil, err
		}
		if !core.UUIDForm.MatchString(l.GetProductId()) {
			return nil, core.Invalid(field+".product_id", "product_id is a product's ID")
		}
		q, err := core.Quantity(field+".quantity", l.GetQuantity())
		if err != nil {
			return nil, err
		}
		lines[i] = receiptLine{ProductID: l.GetProductId(), Quantity: q, Batch: l.GetBatch(), Serials: slices.Clone(l.GetSerials())}
		lineIDs[i] = l.GetLineId()
		pids = append(pids, l.GetProductId())
	}
	date, err := dateOf(m, ctx, in.GetLegalEntityId(), in.GetBusinessDate())
	if err != nil {
		return nil, err
	}
	products, err := masterdata.ProductsOf(m, ctx, pids)
	if err != nil {
		return nil, err
	}
	for i, l := range lines {
		p, ok := products[l.ProductID]
		if !ok || p.Kind != "stock" {
			return nil, core.ErrProduct.New("product " + l.ProductID + " is not a stock product")
		}
		if err := CheckTracking(fmt.Sprintf("lines[%d]", i), p, l.Quantity, l.Batch, l.Serials, true); err != nil {
			return nil, err
		}
	}
	digest, caller := digestOf(in), callerOf(ctx)
	var out []string
	err = m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		done, prior, err := claim(ctx, tx, "purchase_receipt", in.GetReceiptId(), digest)
		if err != nil || done {
			out = prior
			return err
		}
		if _, err := warehouses.UsableWarehouses(ctx, tx, in.GetLegalEntityId(), []string{in.GetWarehouseId()}); err != nil {
			return err
		}
		if out, err = writeReceipt(m, ctx, tx, in.GetLegalEntityId(), in.GetWarehouseId(), date, lines, "purchase_receipt",
			in.GetReference().GetId(), lineIDs); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE purchase_receipt SET movement_ids = $2::uuid[] WHERE id = $1`, in.GetReceiptId(),
			out); err != nil {
			return err
		}
		return core.Change(m, ctx, tx, caller, core.OpSysReceive, core.Movements, out[0], map[string]any{
			"receipt_id": in.GetReceiptId(), "reference": in.GetReference().GetId(), "movements": out})
	})
	if err != nil {
		return nil, err
	}
	numbering.NumberNow(m, ctx)
	return &inventoryv3.ReceiveResponse{ReceiptId: in.GetReceiptId(), MovementIds: out}, nil
}

// claim keeps a call's ID with its digest; done when the ID was kept before with the same digest (prior is what it
// wrote), CONTENT_CONFLICT with another.
func claim(ctx context.Context, tx db.Tx, table, id, digest string) (done bool, prior []string, err error) {
	tag, err := tx.Exec(ctx, `INSERT INTO `+table+` (id, digest) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`, id, digest)
	if err != nil || tag.RowsAffected() == 1 {
		return false, nil, err
	}
	var was string
	col := "movement_ids::text[]"
	if table == "purchase_return" {
		col = "ARRAY[movement_id::text]"
	}
	if err := tx.QueryRow(ctx, `SELECT digest, `+col+` FROM `+table+` WHERE id = $1`, id).Scan(&was, &prior); err != nil {
		return false, nil, err
	}
	if was != digest {
		return false, nil, core.ErrContent.New(id + " was used with other content")
	}
	return true, prior, nil
}

func callerOf(ctx context.Context) string {
	c, _ := grpcx.Caller(ctx)
	return c
}

// origin is the receipt movement a return is made from, locked: returns from one receipt are one at a time.
type origin struct {
	Kind, Product, Warehouse, LegalEntity string
	Batch                                 *string
	Serials                               []string
	Left                                  string // received less returned
}

func originOf(ctx context.Context, tx db.Tx, id string) (origin, error) {
	var o origin
	err := tx.QueryRow(ctx, `SELECT m.kind, m.product_id::text, m.warehouse_id::text, m.legal_entity_id::text, m.batch, m.serials,
			trim_scale(m.quantity - coalesce((SELECT sum(r.quantity) FROM movement r WHERE r.origin_movement_id = m.id), 0))::text
		FROM movement m WHERE m.id = $1 FOR UPDATE`, id).Scan(&o.Kind, &o.Product, &o.Warehouse, &o.LegalEntity, &o.Batch,
		&o.Serials, &o.Left)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, core.ErrReturn.New("no movement " + id)
	}
	return o, err
}

// ReturnPurchase is StockService.Return.
func ReturnPurchase(m *core.Base, ctx context.Context, in *inventoryv3.ReturnRequest) (*inventoryv3.ReturnResponse, error) {
	if err := callID("return_id", in.GetReturnId()); err != nil {
		return nil, err
	}
	if err := callID("line_id", in.GetLineId()); err != nil {
		return nil, err
	}
	if !core.UUIDForm.MatchString(in.GetOriginMovementId()) {
		return nil, core.Invalid("origin_movement_id", "origin_movement_id is a receipt movement's ID")
	}
	if err := reference(in.GetReference(), "purchase_return"); err != nil {
		return nil, err
	}
	qty, err := core.Quantity("quantity", in.GetQuantity())
	if err != nil {
		return nil, err
	}
	digest, caller := digestOf(in), callerOf(ctx)
	// the legal entity's today needs the origin's legal entity: read it first (a movement never changes)
	var le string
	if err := m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, `SELECT legal_entity_id::text FROM movement WHERE id = $1`, in.GetOriginMovementId()).Scan(&le)
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if le == "" {
		return nil, core.ErrReturn.New("no movement " + in.GetOriginMovementId())
	}
	date, err := dateOf(m, ctx, le, in.GetBusinessDate())
	if err != nil {
		return nil, err
	}
	var out string
	err = m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		done, prior, err := claim(ctx, tx, "purchase_return", in.GetReturnId(), digest)
		if err != nil || done {
			if len(prior) == 1 {
				out = prior[0]
			}
			return err
		}
		o, err := originOf(ctx, tx, in.GetOriginMovementId())
		if err != nil {
			return err
		}
		if o.Kind != "receipt" {
			return core.ErrReturn.New("a return is made from a receipt movement, not a " + o.Kind)
		}
		if ok, err := stock.AtLeast(ctx, tx, o.Left, qty); err != nil || !ok {
			if err == nil {
				err = core.ErrReturn.New(fmt.Sprintf("the receipt has %s left to return, not %s", o.Left, qty))
			}
			return err
		}
		serials := slices.Clone(in.GetSerials())
		slices.Sort(serials)
		if err := returnSerials(ctx, tx, o, qty, serials); err != nil {
			return err
		}
		key := stock.Key{Product: o.Product, Warehouse: o.Warehouse}
		pos, err := stock.LockPositions(ctx, tx, o.LegalEntity, []stock.Key{key})
		if err != nil {
			return err
		}
		var avail string
		if err := tx.QueryRow(ctx, `SELECT trim_scale($1::numeric - $2::numeric)::text`, pos[key].OnHand, pos[key].Reserved).
			Scan(&avail); err != nil {
			return err
		}
		if ok, err := stock.AtLeast(ctx, tx, avail, qty); err != nil || !ok {
			if err == nil {
				err = core.ErrReturn.New(fmt.Sprintf("only %s is available in the warehouse now", avail))
			}
			return err
		}
		if o.Batch != nil {
			tag, err := tx.Exec(ctx, `UPDATE stock_lot SET on_hand = on_hand - $4::numeric WHERE product_id = $1
				AND warehouse_id = $2 AND batch = $3 AND on_hand >= $4::numeric`, o.Product, o.Warehouse, *o.Batch, qty)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return core.ErrReturn.New("the batch " + *o.Batch + " has less than that in the warehouse now")
			}
		}
		if serials == nil {
			serials = []string{}
		}
		out = ids.New().String()
		if _, err := tx.Exec(ctx, `INSERT INTO movement (id, kind, product_id, warehouse_id, legal_entity_id, quantity, batch,
			serials, business_date, ref_kind, ref_id, ref_line_id, origin_movement_id) VALUES ($1, 'purchase_return', $2, $3, $4,
			$5::numeric, $6, $7, $8::date, 'purchase_return', $9, $10, $11)`, out, o.Product, o.Warehouse, o.LegalEntity, qty,
			o.Batch, serials, date, in.GetReference().GetId(), in.GetLineId(), in.GetOriginMovementId()); err != nil {
			return err
		}
		if err := stock.MoveStock(m, ctx, tx, key, stock.Neg(qty), "0"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE purchase_return SET movement_id = $2 WHERE id = $1`, in.GetReturnId(), out); err != nil {
			return err
		}
		return core.Change(m, ctx, tx, caller, core.OpSysReceive, core.Movements, out, map[string]any{
			"return_id": in.GetReturnId(), "origin_movement_id": in.GetOriginMovementId(), "quantity": qty})
	})
	if err != nil {
		return nil, err
	}
	numbering.NumberNow(m, ctx)
	return &inventoryv3.ReturnResponse{ReturnId: in.GetReturnId(), MovementId: out}, nil
}

// returnSerials: a serial-tracked receipt is returned by naming exactly quantity of its serial numbers, each still in
// its warehouse (they leave stock); any other receipt names none.
func returnSerials(ctx context.Context, tx db.Tx, o origin, qty string, serials []string) error {
	if len(o.Serials) == 0 {
		if len(serials) > 0 {
			return core.ErrTracking.New("the receipt has no serial numbers")
		}
		return nil
	}
	if qty != fmt.Sprint(len(serials)) || len(slices.Compact(slices.Clone(serials))) != len(serials) { // sorted: duplicates adjacent
		return core.ErrTracking.New(fmt.Sprintf("a serial-tracked return names exactly quantity (%s) distinct serial numbers", qty))
	}
	for _, s := range serials {
		if !slices.Contains(o.Serials, s) {
			return core.ErrReturn.New("serial number " + s + " was not received by that movement")
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE stock_serial SET in_stock = false, updated_at = now() WHERE product_id = $1
		AND warehouse_id = $2 AND serial = ANY($3) AND in_stock`, o.Product, o.Warehouse, serials)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != int64(len(serials)) {
		return core.ErrReturn.New("some of the serial numbers are not in the warehouse now")
	}
	return nil
}

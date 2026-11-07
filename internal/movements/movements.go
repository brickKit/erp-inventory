package movements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"

	core "github.com/brickKit/erp-inventory/v3/internal/core"
	masterdata "github.com/brickKit/erp-inventory/v3/internal/masterdata"
	numbering "github.com/brickKit/erp-inventory/v3/internal/numbering"
	stock "github.com/brickKit/erp-inventory/v3/internal/stock"
	warehouses "github.com/brickKit/erp-inventory/v3/internal/warehouses"

	"github.com/brickKit/be-sdk-go/v3/bizdate"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/idem"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/list"
)

var (
	DateForm  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	labelForm = regexp.MustCompile(`^\S(.{0,62}\S)?$`) // a batch or serial number: 1-64 characters, no outer spaces
)

type receiptLine struct {
	ProductID string   `json:"product_id"`
	Quantity  string   `json:"quantity"`
	Batch     string   `json:"batch"`
	Serials   []string `json:"serials"`
}

// CheckTracking checks a line's batch and serial numbers against the product's tracking policy; quantity is canonical.
// A receipt names the batch; an issue may leave it to the FIFO allocation.
func CheckTracking(field string, p masterdata.Product, qty, batch string, serials []string, batchRequired bool) error {
	switch p.Tracking {
	case "batch":
		if batch == "" && batchRequired {
			return core.ErrTracking.New(fmt.Sprintf("%s: product %s is tracked by batch: the batch is required", field, p.ID))
		}
		if len(serials) > 0 {
			return core.ErrTracking.New(field + ": a batch-tracked product has no serial numbers")
		}
	case "serial":
		if batch != "" {
			return core.ErrTracking.New(field + ": a serial-tracked product has no batch")
		}
		if qty != fmt.Sprint(len(serials)) {
			return core.ErrTracking.New(fmt.Sprintf("%s: a serial-tracked product lists exactly quantity (%s) serial numbers", field, qty))
		}
		seen := map[string]bool{}
		for _, s := range serials {
			if !labelForm.MatchString(s) || seen[s] {
				return core.ErrTracking.New(field + ": serial numbers are distinct, 1-64 characters")
			}
			seen[s] = true
		}
	default:
		if batch != "" || len(serials) > 0 {
			return core.ErrTracking.New(field + ": product " + p.ID + " is not tracked by batch or serial")
		}
	}
	if batch != "" && !labelForm.MatchString(batch) {
		return core.Invalid(field+".batch", "a batch is 1-64 characters")
	}
	return nil
}

// Receive takes stock into one warehouse (seed data and manual receipts), once per idempotency key.
func Receive(m *core.Base, c *httpx.Context) error {
	var in struct {
		WarehouseID  string        `json:"warehouse_id"`
		BusinessDate string        `json:"business_date"`
		Note         string        `json:"note"`
		Lines        []receiptLine `json:"lines"`
	}
	req, err := idem.FromHTTP(c)
	if err != nil {
		return err
	}
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if len(in.Lines) == 0 || len(in.Lines) > core.MaxBatch {
		return core.Invalid("lines", fmt.Sprintf("1-%d lines", core.MaxBatch))
	}
	if in.BusinessDate != "" && !DateForm.MatchString(in.BusinessDate) {
		return core.Invalid("business_date", "business_date is YYYY-MM-DD")
	}
	wh, err := warehouses.Warehouse(m, c.Ctx(), in.WarehouseID)
	if err != nil {
		return err
	}
	ctx, actor, err := core.Check(c, core.OpMoveCreate, core.Scope(wh.LegalEntityID, wh.ID))
	if err != nil {
		return err
	}
	le, err := masterdata.LegalEntityOf(m, ctx, wh.LegalEntityID)
	if err != nil {
		return err
	}
	if in.BusinessDate == "" {
		if in.BusinessDate, err = bizdate.Today(le.Timezone); err != nil {
			return err
		}
	}
	var pids []string
	for i, l := range in.Lines {
		if !core.UUIDForm.MatchString(l.ProductID) {
			return core.Invalid(fmt.Sprintf("lines[%d].product_id", i), "product_id is a product's ID")
		}
		if in.Lines[i].Quantity, err = core.Quantity(fmt.Sprintf("lines[%d].quantity", i), l.Quantity); err != nil {
			return err
		}
		pids = append(pids, l.ProductID)
	}
	products, err := masterdata.ProductsOf(m, ctx, pids)
	if err != nil {
		return err
	}
	for i, l := range in.Lines {
		p, ok := products[l.ProductID]
		if !ok || p.Kind != "stock" {
			return core.ErrProduct.New("product " + l.ProductID + " is not a stock product")
		}
		if err := CheckTracking(fmt.Sprintf("lines[%d]", i), p, l.Quantity, l.Batch, l.Serials, true); err != nil {
			return err
		}
	}
	out, _, err := idem.Do(ctx, m.RT.DB, req, func(ctx context.Context, tx db.Tx) (idem.Outcome, error) {
		if _, err := warehouses.UsableWarehouses(ctx, tx, "", []string{wh.ID}); err != nil {
			return idem.Outcome{}, err
		}
		keys := make([]stock.Key, len(in.Lines))
		for i, l := range in.Lines {
			keys[i] = stock.Key{Product: l.ProductID, Warehouse: wh.ID}
		}
		if _, err := stock.LockPositions(ctx, tx, wh.LegalEntityID, keys); err != nil {
			return idem.Outcome{}, err
		}
		var movements []string
		for i, l := range in.Lines {
			if len(l.Serials) > 0 {
				var taken []string
				if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(serial ORDER BY serial), '{}') FROM stock_serial
					WHERE product_id = $1 AND serial = ANY($2) AND in_stock`, l.ProductID, l.Serials).Scan(&taken); err != nil {
					return idem.Outcome{}, err
				}
				if len(taken) > 0 {
					return idem.Outcome{}, core.ErrTracking.New(fmt.Sprintf("lines[%d]: serial numbers already in stock: %v", i, taken))
				}
				for _, s := range l.Serials {
					if _, err := tx.Exec(ctx, `INSERT INTO stock_serial (product_id, serial, warehouse_id, legal_entity_id, in_stock)
						VALUES ($1, $2, $3, $4, true) ON CONFLICT (product_id, serial) DO UPDATE
						SET warehouse_id = excluded.warehouse_id, legal_entity_id = excluded.legal_entity_id, in_stock = true,
							updated_at = now()`, l.ProductID, s, wh.ID, wh.LegalEntityID); err != nil {
						return idem.Outcome{}, err
					}
				}
			}
			var batch *string
			if l.Batch != "" {
				batch = &l.Batch
				if _, err := tx.Exec(ctx, `INSERT INTO stock_lot (product_id, warehouse_id, legal_entity_id, batch, on_hand)
					VALUES ($1, $2, $3, $4, $5::numeric) ON CONFLICT (product_id, warehouse_id, batch) DO UPDATE
					SET on_hand = stock_lot.on_hand + excluded.on_hand`, l.ProductID, wh.ID, wh.LegalEntityID, l.Batch,
					l.Quantity); err != nil {
					return idem.Outcome{}, err
				}
			}
			id := ids.New().String()
			serials := l.Serials
			if serials == nil {
				serials = []string{}
			}
			slices.Sort(serials)
			if _, err := tx.Exec(ctx, `INSERT INTO movement (id, kind, product_id, warehouse_id, legal_entity_id, quantity,
				batch, serials, business_date, ref_kind, ref_id) VALUES ($1, 'receipt', $2, $3, $4, $5::numeric, $6, $7,
				$8::date, 'receipt', $9)`, id, l.ProductID, wh.ID, wh.LegalEntityID, l.Quantity, batch, serials,
				in.BusinessDate, req.Key); err != nil {
				return idem.Outcome{}, err
			}
			if err := stock.MoveStock(m, ctx, tx, keys[i], l.Quantity, "0"); err != nil {
				return idem.Outcome{}, err
			}
			movements = append(movements, id)
		}
		if err := core.Change(m, ctx, tx, actor, core.OpMoveCreate, core.Movements, movements[0], map[string]any{
			"warehouse_id": wh.ID, "business_date": in.BusinessDate, "note": in.Note, "movements": movements}); err != nil {
			return idem.Outcome{}, err
		}
		res, _ := json.Marshal(map[string]any{"movements": movements})
		return idem.Outcome{Status: 200, Result: res}, nil
	})
	if err != nil {
		return err
	}
	numbering.NumberNow(m, ctx)
	var res struct{ Movements []string }
	_ = json.Unmarshal(out.Result, &res)
	moves, err := movements(m, ctx, res.Movements)
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"movements": moves})
}

// movements reads movements by ID, in the order given.
func movements(m *core.Base, ctx context.Context, idList []string) ([]core.Movement, error) {
	out := []core.Movement{}
	err := m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		out = out[:0]
		for _, id := range idList {
			mv, err := core.One(tx.QueryRow(ctx, `SELECT `+core.MovementCols+` FROM movement m WHERE m.id = $1::uuid`, id), core.ScanMovement)
			if err != nil {
				return err
			}
			out = append(out, mv)
		}
		return nil
	})
	return out, err
}

func GetMovement(m *core.Base, c *httpx.Context) error {
	id := c.Param("id")
	if !core.UUIDForm.MatchString(id) {
		return core.NotFound("movement")
	}
	mv, err := movements(m, c.Ctx(), []string{id})
	if errors.Is(err, core.ErrNone) {
		return core.NotFound("movement")
	}
	if err != nil {
		return err
	}
	if _, _, err := core.Check(c, core.OpMoveRead, core.Scope(mv[0].LegalEntityID, mv[0].WarehouseID)); err != nil {
		return err
	}
	return c.JSON(200, mv[0])
}

var movementList = list.Spec{Operation: core.OpMoveRead, Alias: "m", IDColumn: "id", IDKind: list.UUID,
	Columns: []list.Column{
		{Field: "recorded_at", Column: "recorded_at", Kind: list.Time, Sortable: true},
		{Field: "product_id", Column: "product_id", Kind: list.UUID, Filterable: true},
		{Field: "warehouse_id", Column: "warehouse_id", Kind: list.UUID, Filterable: true},
		{Field: "kind", Column: "kind", Kind: list.Text, Filterable: true},
		{Field: "business_date", Column: "business_date", Kind: list.Date, Filterable: true}}}

func ListMovements(m *core.Base, c *httpx.Context) error {
	f, err := core.Filters(c, "product_id", "warehouse_id", "kind")
	if err != nil {
		return err
	}
	for _, p := range []struct{ name, op string }{{"from", "gte"}, {"to", "lte"}} {
		if v := c.Query(p.name); v != "" {
			if !DateForm.MatchString(v) {
				return core.Invalid(p.name, p.name+" is YYYY-MM-DD")
			}
			f = append(f, list.Filter{Field: "business_date", Op: p.op, Values: []string{v}})
		}
	}
	ctx, q, err := core.Build(c, movementList, f, []list.Order{{Field: "recorded_at", Desc: true}})
	if err != nil {
		return err
	}
	out, next, err := list.Page(ctx, m.RT.DB, q, "movement", core.MovementCols, core.ScanMovement,
		func(v core.Movement) ([]string, string) { return []string{v.RecordedAt}, v.FactID })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"movements": out, "next_cursor": next})
}

package stock

import (
	"context"
	"fmt"
	"sort"
	"time"

	core "github.com/brickKit/erp-inventory/v3/internal/core"

	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	"github.com/jackc/pgx/v5"

	"github.com/brickKit/be-sdk-go/v3/canon"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/list"
)

// Key is one product in one warehouse: the unit of availability.
type Key struct {
	Product, Warehouse string `json:"-"`
}

// LockPositions makes sure each pair has a position row, then locks them all in (product, warehouse) order, the one
// order every transaction uses (RS16). It returns the positions before any change.
func LockPositions(ctx context.Context, tx db.Tx, legalEntity string, keys []Key) (map[Key]core.Position, error) {
	uniq := map[Key]bool{}
	var ks []Key
	for _, k := range keys {
		if !uniq[k] {
			uniq[k] = true
			ks = append(ks, k)
		}
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].Product != ks[j].Product {
			return ks[i].Product < ks[j].Product
		}
		return ks[i].Warehouse < ks[j].Warehouse
	})
	out := map[Key]core.Position{}
	for _, k := range ks {
		if _, err := tx.Exec(ctx, `INSERT INTO stock_position (id, product_id, warehouse_id, legal_entity_id)
			VALUES ($1, $2, $3, $4) ON CONFLICT (product_id, warehouse_id) DO NOTHING`, ids.New().String(), k.Product,
			k.Warehouse, legalEntity); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM stock_position WHERE product_id = $1 AND warehouse_id = $2 FOR UPDATE`,
			k.Product, k.Warehouse); err != nil {
			return nil, err
		}
		p, err := core.GetPosition(ctx, tx, k.Product, k.Warehouse)
		if err != nil {
			return nil, err
		}
		out[k] = p
	}
	return out, nil
}

// MoveStock adds on hand and reserved (either may be negative) to a locked position and publishes its new state.
func MoveStock(m *core.Base, ctx context.Context, tx db.Tx, k Key, onHand, reserved string) error {
	if _, err := tx.Exec(ctx, `UPDATE stock_position SET on_hand = on_hand + $3::numeric, reserved = reserved + $4::numeric,
		version = version + 1, updated_at = now() WHERE product_id = $1 AND warehouse_id = $2`, k.Product, k.Warehouse,
		onHand, reserved); err != nil {
		return err
	}
	p, err := core.GetPosition(ctx, tx, k.Product, k.Warehouse)
	if err != nil {
		return err
	}
	return core.Emit(m, ctx, tx, core.PositionChanged, p.ID, p.Version, p)
}

// Neg is the negative of a decimal string.
func Neg(q string) string { return "-" + q }

// AtLeast reports whether a >= b for decimal strings, in the database (no float anywhere).
func AtLeast(ctx context.Context, tx db.Tx, a, b string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT $1::numeric >= $2::numeric`, a, b).Scan(&ok)
	return ok, err
}

var positionList = list.Spec{Operation: core.OpStockRead, Alias: "s", IDColumn: "id", IDKind: list.UUID,
	Columns: []list.Column{
		{Field: "warehouse_id", Column: "warehouse_id", Kind: list.UUID, Sortable: true, Filterable: true},
		{Field: "product_id", Column: "product_id", Kind: list.UUID, Sortable: true, Filterable: true}}}

var lotList = list.Spec{Operation: core.OpStockRead, Alias: "l", IDColumn: "batch", IDKind: list.Text,
	Columns: []list.Column{
		{Field: "first_received_at", Column: "first_received_at", Kind: list.Time, Sortable: true},
		{Field: "product_id", Column: "product_id", Kind: list.UUID, Filterable: true},
		{Field: "warehouse_id", Column: "warehouse_id", Kind: list.UUID, Filterable: true}}}

var serialList = list.Spec{Operation: core.OpStockRead, Alias: "t", IDColumn: "serial", IDKind: list.Text,
	Columns: []list.Column{
		{Field: "product_id", Column: "product_id", Kind: list.UUID, Filterable: true},
		{Field: "warehouse_id", Column: "warehouse_id", Kind: list.UUID, Filterable: true},
		{Field: "in_stock", Column: "in_stock", Kind: "boolean", Filterable: true}}}

func ListPositions(m *core.Base, c *httpx.Context) error {
	f, err := core.Filters(c, "warehouse_id", "product_id")
	if err != nil {
		return err
	}
	if c.Query("available_only") == "true" {
		f = append(f, list.Filter{Field: "available", Op: "gt", Values: []string{"0"}})
	}
	spec := positionList
	spec.Columns = append(spec.Columns, list.Column{Field: "available", Column: "available", Kind: list.Decimal, Filterable: true})
	ctx, q, err := core.Build(c, spec, f, []list.Order{{Field: "warehouse_id"}, {Field: "product_id"}})
	if err != nil {
		return err
	}
	// "available" is a computed column: the page reads from a subquery that has it
	from := "(SELECT *, on_hand - reserved AS available FROM stock_position)"
	out, next, err := list.Page(ctx, m.RT.DB, q, from, core.PositionCols, core.ScanPosition,
		func(p core.Position) ([]string, string) { return []string{p.WarehouseID, p.ProductID}, p.ID })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"positions": out, "next_cursor": next})
}

type lotOut struct {
	Batch           string `json:"batch"`
	OnHand          string `json:"on_hand"`
	FirstReceivedAt string `json:"first_received_at"`
}

func ListLots(m *core.Base, c *httpx.Context) error {
	if c.Query("product_id") == "" || c.Query("warehouse_id") == "" {
		return core.Invalid("product_id", "product_id and warehouse_id are required")
	}
	f, err := core.Filters(c, "product_id", "warehouse_id")
	if err != nil {
		return err
	}
	f = append(f, list.Filter{Field: "on_hand", Op: "gt", Values: []string{"0"}})
	spec := lotList
	spec.Columns = append(spec.Columns, list.Column{Field: "on_hand", Column: "on_hand", Kind: list.Decimal, Filterable: true})
	ctx, q, err := core.Build(c, spec, f, []list.Order{{Field: "first_received_at"}})
	if err != nil {
		return err
	}
	out, next, err := list.Page(ctx, m.RT.DB, q, "stock_lot", `l.batch, trim_scale(l.on_hand)::text, l.first_received_at`,
		func(row pgx.Row) (lotOut, error) {
			var v lotOut
			var at time.Time
			err := row.Scan(&v.Batch, &v.OnHand, &at)
			v.FirstReceivedAt = canon.Time(at)
			return v, err
		}, func(v lotOut) ([]string, string) { return []string{v.FirstReceivedAt}, v.Batch })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"lots": out, "next_cursor": next})
}

type serialOut struct {
	Serial      string `json:"serial"`
	WarehouseID string `json:"warehouse_id"`
	InStock     bool   `json:"in_stock"`
}

func ListSerials(m *core.Base, c *httpx.Context) error {
	if c.Query("product_id") == "" {
		return core.Invalid("product_id", "product_id is required")
	}
	f, err := core.Filters(c, "product_id", "warehouse_id")
	if err != nil {
		return err
	}
	if v := c.Query("in_stock"); v != "" {
		if v != "true" && v != "false" {
			return core.Invalid("in_stock", "in_stock is true or false")
		}
		f = append(f, list.Filter{Field: "in_stock", Op: "eq", Values: []string{v}})
	}
	ctx, q, err := core.Build(c, serialList, f, nil)
	if err != nil {
		return err
	}
	out, next, err := list.Page(ctx, m.RT.DB, q, "stock_serial", `t.serial, t.warehouse_id::text, t.in_stock`,
		func(row pgx.Row) (serialOut, error) {
			var v serialOut
			return v, row.Scan(&v.Serial, &v.WarehouseID, &v.InStock)
		}, func(v serialOut) ([]string, string) { return []string{}, v.Serial })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"serials": out, "next_cursor": next})
}

// Positions answers on hand, reserved and available by product and warehouse (the system surface).
func Positions(m *core.Base, ctx context.Context, in *inventoryv3.PositionsRequest) (*inventoryv3.PositionsResponse, error) {
	if len(in.GetKeys()) > core.MaxBatch {
		return nil, core.Invalid("keys", fmt.Sprintf("at most %d pairs a call", core.MaxBatch))
	}
	out := &inventoryv3.PositionsResponse{}
	err := m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		out.Positions = nil
		for _, k := range in.GetKeys() {
			p, err := core.GetPosition(ctx, tx, k.GetProductId(), k.GetWarehouseId())
			switch {
			case err == core.ErrNone:
				out.Positions = append(out.Positions, &inventoryv3.Position{ProductId: k.GetProductId(),
					WarehouseId: k.GetWarehouseId(), OnHand: "0", Reserved: "0", Available: "0"})
			case err != nil:
				return err
			default:
				out.Positions = append(out.Positions, &inventoryv3.Position{ProductId: p.ProductID, WarehouseId: p.WarehouseID,
					OnHand: p.OnHand, Reserved: p.Reserved, Available: p.Available, Version: p.Version})
			}
		}
		return nil
	})
	return out, err
}

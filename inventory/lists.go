package inventory

import (
	"context"
	"fmt"
	"time"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/canon"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/list"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	"github.com/jackc/pgx/v5"
)

// The lists of the user surface (protocol 14: cursor pages; the filters and keyword declared here). Every field is
// ordinary, so a row is listed whole or not at all; the legal entity and warehouse dimensions compile into the WHERE.
var (
	warehouseList = list.Spec{Operation: OpWhRead, Alias: "w", IDColumn: "id", IDKind: list.UUID,
		Columns: []list.Column{
			{Field: "code", Column: "code", Kind: list.Text, Sortable: true},
			{Field: "state", Column: "state", Kind: list.Text, Filterable: true},
			{Field: "legal_entity_id", Column: "legal_entity_id", Kind: list.UUID, Filterable: true}},
		Search: []list.SearchColumn{{Name: "code_name", Column: "search", Fields: []string{"code", "name"}}}}
	positionList = list.Spec{Operation: OpStockRead, Alias: "s", IDColumn: "id", IDKind: list.UUID,
		Columns: []list.Column{
			{Field: "warehouse_id", Column: "warehouse_id", Kind: list.UUID, Sortable: true, Filterable: true},
			{Field: "product_id", Column: "product_id", Kind: list.UUID, Sortable: true, Filterable: true}}}
	movementList = list.Spec{Operation: OpMoveRead, Alias: "m", IDColumn: "id", IDKind: list.UUID,
		Columns: []list.Column{
			{Field: "recorded_at", Column: "recorded_at", Kind: list.Time, Sortable: true},
			{Field: "product_id", Column: "product_id", Kind: list.UUID, Filterable: true},
			{Field: "warehouse_id", Column: "warehouse_id", Kind: list.UUID, Filterable: true},
			{Field: "kind", Column: "kind", Kind: list.Text, Filterable: true},
			{Field: "business_date", Column: "business_date", Kind: list.Date, Filterable: true}}}
	lotList = list.Spec{Operation: OpStockRead, Alias: "l", IDColumn: "batch", IDKind: list.Text,
		Columns: []list.Column{
			{Field: "first_received_at", Column: "first_received_at", Kind: list.Time, Sortable: true},
			{Field: "product_id", Column: "product_id", Kind: list.UUID, Filterable: true},
			{Field: "warehouse_id", Column: "warehouse_id", Kind: list.UUID, Filterable: true}}}
	serialList = list.Spec{Operation: OpStockRead, Alias: "t", IDColumn: "serial", IDKind: list.Text,
		Columns: []list.Column{
			{Field: "product_id", Column: "product_id", Kind: list.UUID, Filterable: true},
			{Field: "warehouse_id", Column: "warehouse_id", Kind: list.UUID, Filterable: true},
			{Field: "in_stock", Column: "in_stock", Kind: "boolean", Filterable: true}}}
)

// filters turns the given query parameters into equality filters, checking the ID ones.
func filters(c *httpx.Context, names ...string) ([]list.Filter, error) {
	var f []list.Filter
	for _, k := range names {
		v := c.Query(k)
		if v == "" {
			continue
		}
		if (k == "product_id" || k == "warehouse_id" || k == "legal_entity_id") && !uuidForm.MatchString(v) {
			return nil, invalid(k, k+" is an ID")
		}
		f = append(f, list.Filter{Field: k, Op: "eq", Values: []string{v}})
	}
	return f, nil
}

func build(c *httpx.Context, spec list.Spec, f []list.Filter, sort []list.Order) (context.Context, *list.Query, error) {
	limit, err := list.ParseLimit(c.Query("limit"))
	if err != nil {
		return nil, nil, err
	}
	s, _ := sdkauthz.From(c.Ctx())
	return list.Build(c.Ctx(), s, spec, list.Request{Filters: f, Q: c.Query("q"), Limit: limit, Cursor: c.Query("cursor"),
		Sort: sort})
}

func (m *Module) listWarehouses(c *httpx.Context) error {
	f, err := filters(c, "state", "legal_entity_id")
	if err != nil {
		return err
	}
	ctx, q, err := build(c, warehouseList, f, []list.Order{{Field: "code"}})
	if err != nil {
		return err
	}
	out, next, err := list.Page(ctx, m.rt.DB, q, "warehouse", warehouseCols, scanWarehouse,
		func(w Warehouse) ([]string, string) { return []string{w.Code}, w.ID })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"warehouses": out, "next_cursor": next})
}

func (m *Module) listPositions(c *httpx.Context) error {
	f, err := filters(c, "warehouse_id", "product_id")
	if err != nil {
		return err
	}
	if c.Query("available_only") == "true" {
		f = append(f, list.Filter{Field: "available", Op: "gt", Values: []string{"0"}})
	}
	spec := positionList
	spec.Columns = append(spec.Columns, list.Column{Field: "available", Column: "available", Kind: list.Decimal, Filterable: true})
	ctx, q, err := build(c, spec, f, []list.Order{{Field: "warehouse_id"}, {Field: "product_id"}})
	if err != nil {
		return err
	}
	// "available" is a computed column: the page reads from a subquery that has it
	from := "(SELECT *, on_hand - reserved AS available FROM stock_position)"
	out, next, err := list.Page(ctx, m.rt.DB, q, from, positionCols, scanPosition,
		func(p Position) ([]string, string) { return []string{p.WarehouseID, p.ProductID}, p.ID })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"positions": out, "next_cursor": next})
}

func (m *Module) listMovements(c *httpx.Context) error {
	f, err := filters(c, "product_id", "warehouse_id", "kind")
	if err != nil {
		return err
	}
	for _, p := range []struct{ name, op string }{{"from", "gte"}, {"to", "lte"}} {
		if v := c.Query(p.name); v != "" {
			if !dateForm.MatchString(v) {
				return invalid(p.name, p.name+" is YYYY-MM-DD")
			}
			f = append(f, list.Filter{Field: "business_date", Op: p.op, Values: []string{v}})
		}
	}
	ctx, q, err := build(c, movementList, f, []list.Order{{Field: "recorded_at", Desc: true}})
	if err != nil {
		return err
	}
	out, next, err := list.Page(ctx, m.rt.DB, q, "movement", movementCols, scanMovement,
		func(v Movement) ([]string, string) { return []string{v.RecordedAt}, v.FactID })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"movements": out, "next_cursor": next})
}

type lotOut struct {
	Batch           string `json:"batch"`
	OnHand          string `json:"on_hand"`
	FirstReceivedAt string `json:"first_received_at"`
}

func (m *Module) listLots(c *httpx.Context) error {
	if c.Query("product_id") == "" || c.Query("warehouse_id") == "" {
		return invalid("product_id", "product_id and warehouse_id are required")
	}
	f, err := filters(c, "product_id", "warehouse_id")
	if err != nil {
		return err
	}
	f = append(f, list.Filter{Field: "on_hand", Op: "gt", Values: []string{"0"}})
	spec := lotList
	spec.Columns = append(spec.Columns, list.Column{Field: "on_hand", Column: "on_hand", Kind: list.Decimal, Filterable: true})
	ctx, q, err := build(c, spec, f, []list.Order{{Field: "first_received_at"}})
	if err != nil {
		return err
	}
	out, next, err := list.Page(ctx, m.rt.DB, q, "stock_lot", `l.batch, trim_scale(l.on_hand)::text, l.first_received_at`,
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

func (m *Module) listSerials(c *httpx.Context) error {
	if c.Query("product_id") == "" {
		return invalid("product_id", "product_id is required")
	}
	f, err := filters(c, "product_id", "warehouse_id")
	if err != nil {
		return err
	}
	if v := c.Query("in_stock"); v != "" {
		if v != "true" && v != "false" {
			return invalid("in_stock", "in_stock is true or false")
		}
		f = append(f, list.Filter{Field: "in_stock", Op: "eq", Values: []string{v}})
	}
	ctx, q, err := build(c, serialList, f, nil)
	if err != nil {
		return err
	}
	out, next, err := list.Page(ctx, m.rt.DB, q, "stock_serial", `t.serial, t.warehouse_id::text, t.in_stock`,
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
func (m *Module) Positions(ctx context.Context, in *inventoryv3.PositionsRequest) (*inventoryv3.PositionsResponse, error) {
	if len(in.GetKeys()) > MaxBatch {
		return nil, invalid("keys", fmt.Sprintf("at most %d pairs a call", MaxBatch))
	}
	out := &inventoryv3.PositionsResponse{}
	err := m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		out.Positions = nil
		for _, k := range in.GetKeys() {
			p, err := getPosition(ctx, tx, k.GetProductId(), k.GetWarehouseId())
			switch {
			case err == errNone:
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

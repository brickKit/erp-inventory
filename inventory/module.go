package inventory

import (
	"context"
	"time"

	"github.com/brickKit/be-sdk-go/v3/grpcx"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/runtime"
	"github.com/brickKit/be-sdk-go/v3/work"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	branches "github.com/brickKit/erp-inventory/v3/internal/branches"
	core "github.com/brickKit/erp-inventory/v3/internal/core"
	movements "github.com/brickKit/erp-inventory/v3/internal/movements"
	numbering "github.com/brickKit/erp-inventory/v3/internal/numbering"
	stock "github.com/brickKit/erp-inventory/v3/internal/stock"
	warehouses "github.com/brickKit/erp-inventory/v3/internal/warehouses"
)

// Module is the inventory component as the launcher and the shells run it: it only assembles the business modules
// under internal/ (their map is internal/AGENTS.md); the gRPC services answer through them (services.go).
type Module struct {
	inventoryv3.UnimplementedReservationServiceServer
	inventoryv3.UnimplementedStockServiceServer
	b *core.Base
}

// New builds the module from its runtime.
func New(rt *runtime.Runtime) (*Module, error) { return &Module{b: &core.Base{RT: rt}}, nil }

// Routes registers the user surface (contract/openapi.yaml).
func (m *Module) Routes(r *httpx.Routes) error {
	add := func(method, path, op string, h httpx.Handler) {
		r.Add(httpx.Route{Method: method, Path: path, Operation: op, Handle: h})
	}
	h := m.handler
	add("GET", "/api/inventory/warehouses", core.OpWhRead, h(warehouses.ListWarehouses))
	add("POST", "/api/inventory/warehouses", core.OpWhManage, h(warehouses.CreateWarehouse))
	add("GET", "/api/inventory/warehouses/:id", core.OpWhRead, h(warehouses.GetWarehouse))
	add("PATCH", "/api/inventory/warehouses/:id", core.OpWhManage, h(warehouses.UpdateWarehouse))
	add("POST", "/api/inventory/receipts", core.OpMoveCreate, h(movements.Receive))
	add("GET", "/api/inventory/movements", core.OpMoveRead, h(movements.ListMovements))
	add("GET", "/api/inventory/movements/:id", core.OpMoveRead, h(movements.GetMovement))
	add("GET", "/api/inventory/positions", core.OpStockRead, h(stock.ListPositions))
	add("GET", "/api/inventory/lots", core.OpStockRead, h(stock.ListLots))
	add("GET", "/api/inventory/serials", core.OpStockRead, h(stock.ListSerials))
	add("GET", "/api/inventory/branches/:id", core.OpStockRead, h(branches.GetBranchHTTP))
	// in the contract, not in this version (D75)
	add("POST", "/api/inventory/transfers", core.OpMoveCreate, core.Unsupported)
	add("POST", "/api/inventory/adjustments", core.OpMoveAdjust, core.Unsupported)
	return nil
}

// Services registers the system surface: the reservation branches and the issue for erp/sales (protocol 04), the
// positions for any component.
func (m *Module) Services(s *grpcx.Services) error {
	s.Register(&inventoryv3.ReservationService_ServiceDesc, m, map[string]grpcx.Method{
		"Hold":    {Operation: core.OpSysReserve, Callers: []string{core.Sales}},
		"Confirm": {Operation: core.OpSysReserve, Callers: []string{core.Sales}},
		"Cancel":  {Operation: core.OpSysReserve, Callers: []string{core.Sales}},
		"Query":   {Operation: core.OpSysReserve, Callers: []string{core.Sales}},
		"Adjust":  {Operation: core.OpSysReserve, Callers: []string{core.Sales}}})
	s.Register(&inventoryv3.StockService_ServiceDesc, m, map[string]grpcx.Method{
		"Issue":     {Operation: core.OpSysIssue, Callers: []string{core.Sales}},
		"Receive":   {Operation: core.OpSysReceive, Callers: []string{core.Purchase}},
		"Return":    {Operation: core.OpSysReceive, Callers: []string{core.Purchase}},
		"Positions": {Operation: core.OpSysRead, Callers: []string{grpcx.AnyComponent}},
		"Movements": {Operation: core.OpSysRead, Callers: []string{core.Costing}}})
	return nil
}

// Jobs declares the background work (protocol 01): the numbering of movements, the expiry of holds.
func (m *Module) Jobs(j *work.Jobs) error {
	j.Add(work.Job{Name: "number-movements", Kind: work.Singleton, Effect: work.Local, Interval: time.Second,
		MaxAttempts: work.Unlimited, Handle: func(ctx context.Context, _ *work.Run) error { return numbering.Number(m.b, ctx) }})
	j.Add(work.Job{Name: "expire-holds", Kind: work.Singleton, Effect: work.Local, Interval: 10 * time.Second,
		MaxAttempts: work.Unlimited, Handle: func(ctx context.Context, r *work.Run) error { return branches.ExpireHolds(m.b, ctx, r) }})
	return nil
}

// handler adapts a module's handler, which takes the shared base first, to what the SDK registers.
func (m *Module) handler(f func(*core.Base, *httpx.Context) error) httpx.Handler {
	return func(c *httpx.Context) error { return f(m.b, c) }
}

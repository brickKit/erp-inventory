package inventory

import (
	"context"

	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	branches "github.com/brickKit/erp-inventory/v3/internal/branches"
	issue "github.com/brickKit/erp-inventory/v3/internal/issue"
	stock "github.com/brickKit/erp-inventory/v3/internal/stock"
)

// The gRPC methods of the system surface answer through internal/branches, internal/issue and internal/stock.

// Hold is ReservationService.Hold.
func (m *Module) Hold(ctx context.Context, in *inventoryv3.HoldRequest) (*inventoryv3.HoldResponse, error) {
	return branches.Hold(m.b, ctx, in)
}

// Confirm is ReservationService.Confirm.
func (m *Module) Confirm(ctx context.Context, in *inventoryv3.ConfirmRequest) (*inventoryv3.ConfirmResponse, error) {
	return branches.Confirm(m.b, ctx, in)
}

// Cancel is ReservationService.Cancel.
func (m *Module) Cancel(ctx context.Context, in *inventoryv3.CancelRequest) (*inventoryv3.CancelResponse, error) {
	return branches.Cancel(m.b, ctx, in)
}

// Query is ReservationService.Query.
func (m *Module) Query(ctx context.Context, in *inventoryv3.QueryRequest) (*inventoryv3.QueryResponse, error) {
	return branches.Query(m.b, ctx, in)
}

// Adjust is ReservationService.Adjust.
func (m *Module) Adjust(ctx context.Context, in *inventoryv3.AdjustRequest) (*inventoryv3.AdjustResponse, error) {
	return branches.Adjust(m.b, ctx, in)
}

// Issue is StockService.Issue.
func (m *Module) Issue(ctx context.Context, in *inventoryv3.IssueRequest) (*inventoryv3.IssueResponse, error) {
	return issue.Issue(m.b, ctx, in)
}

// Positions is StockService.Positions.
func (m *Module) Positions(ctx context.Context, in *inventoryv3.PositionsRequest) (*inventoryv3.PositionsResponse, error) {
	return stock.Positions(m.b, ctx, in)
}

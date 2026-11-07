//go:build integration

package itest

import (
	"github.com/brickKit/be-sdk-go/v3/ids"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
)

// issue: RS19: the issue consumes the committed branch in the same transaction; the same delivery again issues
// nothing more; batches go oldest first.
func (fx *fixture) issue() {
	t, ctx := fx.t, fx.ctx
	// RS19: the issue consumes the committed branch in the same transaction; batches go oldest first
	cf, err := fx.res.Confirm(ctx, &inventoryv3.ConfirmRequest{BranchId: fx.order})
	require.NoError(t, err)
	require.Equal(t, inventoryv3.BranchState_BRANCH_STATE_COMMITTED, cf.Branch.State)
	delivery := ids.New().String()
	req := &inventoryv3.IssueRequest{ConsumptionId: delivery, BranchId: fx.order, BusinessDate: "2026-10-21",
		Reference: &inventoryv3.Reference{Kind: "sales_order", Id: "SO-1"},
		Lines:     []*inventoryv3.IssueLine{{ProductId: fx.tea, WarehouseId: fx.whSH, Quantity: "2"}}}
	is, err := fx.stockc.Issue(ctx, req)
	require.NoError(t, err)
	require.True(t, is.Issued, is.Problems)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_CONSUMED, is.BranchState)
	pos := fx.position(fx.tea, fx.whSH)
	assert.Equal(t, "8", pos["on_hand"])
	assert.Equal(t, "5", pos["available"], "the issue took what was held: available unchanged")
	again2, err := fx.stockc.Issue(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, is.MovementIds, again2.MovementIds, "the same delivery again issues nothing more")
	assert.Equal(t, "8", fx.position(fx.tea, fx.whSH)["on_hand"])
	_, lots := fx.alice.do("GET", "/api/inventory/lots?product_id="+fx.tea+"&warehouse_id="+fx.whSH, nil)
	first0 := lots["lots"].([]any)[0].(map[string]any)
	assert.Equal(t, "T-OLD", first0["batch"])
	assert.Equal(t, "2", first0["on_hand"], "FIFO: the older batch went first")
}

// serialIssue: the story: a serial-tracked product is issued only by naming its serial numbers; one of two issued,
// the branch goes on.
func (fx *fixture) serialIssue() {
	t, ctx := fx.t, fx.ctx
	// the story: a serial-tracked product is issued only by naming its serial numbers
	sn := ids.New().String()
	fx.hold(sn, line(fx.phone, fx.whSH, "2"))
	_, _ = fx.res.Confirm(ctx, &inventoryv3.ConfirmRequest{BranchId: sn})
	is, err := fx.stockc.Issue(ctx, &inventoryv3.IssueRequest{ConsumptionId: ids.New().String(), BranchId: sn, BusinessDate: "2026-10-21",
		Lines: []*inventoryv3.IssueLine{{ProductId: fx.phone, WarehouseId: fx.whSH, Quantity: "2"}}})
	require.NoError(t, err)
	assert.False(t, is.Issued)
	assert.Equal(t, "serial_required", is.Problems[0].Reason)
	is, err = fx.stockc.Issue(ctx, &inventoryv3.IssueRequest{ConsumptionId: ids.New().String(), BranchId: sn, BusinessDate: "2026-10-21",
		Lines: []*inventoryv3.IssueLine{{ProductId: fx.phone, WarehouseId: fx.whSH, Quantity: "2", Serials: []string{"SN-1", "SN-9"}}}})
	require.NoError(t, err)
	assert.Equal(t, "serial_not_found", is.Problems[0].Reason)
	is, err = fx.stockc.Issue(ctx, &inventoryv3.IssueRequest{ConsumptionId: ids.New().String(), BranchId: sn, BusinessDate: "2026-10-21",
		Lines: []*inventoryv3.IssueLine{{ProductId: fx.phone, WarehouseId: fx.whSH, Quantity: "1", Serials: []string{"SN-3"}}}})
	require.NoError(t, err)
	require.True(t, is.Issued, is.Problems)
	assert.Equal(t, inventoryv3.BranchState_BRANCH_STATE_COMMITTED, is.BranchState, "one of two issued: the branch goes on")
}

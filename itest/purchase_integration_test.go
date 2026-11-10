//go:build integration

package itest

import (
	"github.com/brickKit/be-sdk-go/v3/ids"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// purchases (3.0.37, the purchase component's receipts and returns): Receive takes a purchase receipt in, idempotent by
// receipt ID, each movement carrying the receipt line; Return issues a purchase return from an original receipt
// movement's warehouse and batch, at most what it received and has not returned; both answer the purchase component
// only.
func (fx *fixture) purchases() {
	t, ctx := fx.t, fx.ctx
	recv := func(id string, lines ...*inventoryv3.ReceiveLine) *inventoryv3.ReceiveRequest {
		return &inventoryv3.ReceiveRequest{ReceiptId: id, LegalEntityId: fx.le, WarehouseId: fx.whSH, BusinessDate: "2026-10-22",
			Reference: &inventoryv3.Reference{Kind: "purchase_receipt", Id: "PO-1", Revision: 1}, Lines: lines}
	}
	_, err := fx.stockc.Receive(ctx, recv(ids.New().String(), &inventoryv3.ReceiveLine{LineId: "L0", ProductId: fx.rice, Quantity: "1"}))
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "the purchase component only: %v", err)

	rice0, tea0 := fx.position(fx.rice, fx.whSH)["on_hand"], fx.position(fx.tea, fx.whSH)["on_hand"]
	rid := ids.New().String()
	req := recv(rid, &inventoryv3.ReceiveLine{LineId: "L1", ProductId: fx.rice, Quantity: "10"},
		&inventoryv3.ReceiveLine{LineId: "L2", ProductId: fx.tea, Quantity: "4", Batch: "T-PO"},
		&inventoryv3.ReceiveLine{LineId: "L3", ProductId: fx.phone, Quantity: "2", Serials: []string{"P-PO-1", "P-PO-2"}})
	got, err := fx.purchase.Receive(ctx, req)
	require.NoError(t, err)
	require.Len(t, got.GetMovementIds(), 3)
	again, err := fx.purchase.Receive(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, got.GetMovementIds(), again.GetMovementIds(), "the same receipt again receives nothing more")
	other := recv(rid, &inventoryv3.ReceiveLine{LineId: "L1", ProductId: fx.rice, Quantity: "11"})
	_, err = fx.purchase.Receive(ctx, other)
	assert.Error(t, err, "the same receipt with other content is refused")
	assert.Equal(t, plus(rice0, 10), fx.position(fx.rice, fx.whSH)["on_hand"])
	assert.Equal(t, plus(tea0, 4), fx.position(fx.tea, fx.whSH)["on_hand"])
	_, mv := fx.alice.do("GET", "/api/inventory/movements/"+got.GetMovementIds()[0], nil)
	assert.Equal(t, "receipt", mv["kind"])
	assert.Equal(t, map[string]any{"kind": "purchase_receipt", "id": "PO-1", "line_id": "L1"}, mv["reference"], "the kind, the order and the receipt line (a movement records no revision)")
	_, bad := fx.purchase.Receive(ctx, recv(ids.New().String(), &inventoryv3.ReceiveLine{LineId: "L9", ProductId: fx.tea, Quantity: "1"}))
	assert.Error(t, bad, "the tracking policy as for any receipt: the batch is required")

	// returns: from the original receipt's warehouse and batch, at most what it received and has not returned
	ret := func(id, origin, qty string, serials ...string) (*inventoryv3.ReturnResponse, error) {
		return fx.purchase.Return(ctx, &inventoryv3.ReturnRequest{ReturnId: id, OriginMovementId: origin, Quantity: qty,
			Serials: serials, BusinessDate: "2026-10-23", LineId: "R1",
			Reference: &inventoryv3.Reference{Kind: "purchase_return", Id: "PR-1"}})
	}
	// another receipt first: enough available in the warehouse, and a serial number in stock that this receipt did not
	// bring, so each refusal below is its own rule's, not a shortage
	more, err := fx.purchase.Receive(ctx, recv(ids.New().String(), &inventoryv3.ReceiveLine{LineId: "L4", ProductId: fx.rice, Quantity: "20"},
		&inventoryv3.ReceiveLine{LineId: "L5", ProductId: fx.phone, Quantity: "1", Serials: []string{"P-OTHER"}}))
	require.NoError(t, err)
	require.Len(t, more.GetMovementIds(), 2)
	r1 := ids.New().String()
	back, err := ret(r1, got.GetMovementIds()[0], "4")
	require.NoError(t, err)
	_, rm := fx.alice.do("GET", "/api/inventory/movements/"+back.GetMovementId(), nil)
	assert.Equal(t, "purchase_return", rm["kind"])
	assert.Equal(t, got.GetMovementIds()[0], rm["origin_movement_id"])
	assert.Equal(t, plus(rice0, 26), fx.position(fx.rice, fx.whSH)["on_hand"])
	twice, err := ret(r1, got.GetMovementIds()[0], "4")
	require.NoError(t, err)
	assert.Equal(t, back.GetMovementId(), twice.GetMovementId(), "the same return again issues nothing more")
	assert.Equal(t, plus(rice0, 26), fx.position(fx.rice, fx.whSH)["on_hand"])
	_, err = ret(ids.New().String(), got.GetMovementIds()[0], "7")
	assert.Error(t, err, "6 left to return from that receipt")
	_, err = ret(ids.New().String(), got.GetMovementIds()[0], "6")
	require.NoError(t, err, "the rest")

	// the batch of the original
	_, err = ret(ids.New().String(), got.GetMovementIds()[1], "3")
	require.NoError(t, err)
	_, lots := fx.alice.do("GET", "/api/inventory/lots?product_id="+fx.tea+"&warehouse_id="+fx.whSH, nil)
	for _, l := range lots["lots"].([]any) {
		if l.(map[string]any)["batch"] == "T-PO" {
			assert.Equal(t, "1", l.(map[string]any)["on_hand"], "returned from its own batch")
		}
	}
	// serial numbers: named, from the original receipt
	_, err = ret(ids.New().String(), got.GetMovementIds()[2], "1")
	assert.Error(t, err, "a serial-tracked return names its serial numbers")
	_, err = ret(ids.New().String(), got.GetMovementIds()[2], "1", "P-OTHER")
	assert.Error(t, err, "only serial numbers the original received (P-OTHER is in stock, from another receipt)")
	_, err = ret(ids.New().String(), got.GetMovementIds()[2], "1", "P-PO-2")
	require.NoError(t, err)
	// not a receipt: no return from it
	_, err = ret(ids.New().String(), back.GetMovementId(), "1")
	assert.Error(t, err, "a return is from a receipt")
}

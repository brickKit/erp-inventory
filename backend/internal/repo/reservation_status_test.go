package repo

import (
	"context"
	"testing"
)

// TestGetReservationStatus_别的命令的幂等键当作查不到：按 idempotency_key 查状态，
// 查的是"这个 Reserve 请求有没有提交"。同一张 command_idempotency 表里还记着
// Receive / ConfirmIssue / CancelReservation 的键，它们的 result_id 是流水 id
// 或"状态|流水 id"，不是 reservation_id——拿它们去解析，要么报错，要么（流水 id
// 恰好等于某个 reservation_id 时）报出一个毫不相干的预留的状态。这些键对状态
// 查询来说就是"没有这个 Reserve"：NOT_FOUND，不报错。
func TestGetReservationStatus_别的命令的幂等键当作查不到(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	allowed := allowedWarehouses(t, east)
	pid := uniqueProductID("status-foreign-key")

	recvKey := "test-recv-foreign-" + pid
	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: recvKey, ProductID: pid, WarehouseID: east, Qty: "10", AllowedWarehouseIDs: allowed,
	}); err != nil {
		t.Fatal(err)
	}
	rid, err := r.Reserve(ctx, ReserveInput{
		IdempotencyKey: "test-reserve-foreign-" + pid, OrderID: "order-foreign",
		Items: []ReserveItem{{ProductID: pid, WarehouseID: east, Qty: "2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	confirmKey := "test-confirm-foreign-" + pid
	if _, _, err := r.ConfirmIssue(ctx, ConfirmIssueInput{IdempotencyKey: confirmKey, ReservationID: rid}); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{recvKey, confirmKey} {
		status, orderID, resolved, err := r.GetReservationStatus(ctx, "", key)
		if err != nil {
			t.Fatalf("idempotency_key=%q（不是 Reserve 的键）不该报错：%v", key, err)
		}
		if status != StatusUnspecified || orderID != "" || resolved != "" {
			t.Fatalf("idempotency_key=%q 不是 Reserve 的键，应是 NOT_FOUND，实际 status=%q order_id=%q reservation_id=%q",
				key, status, orderID, resolved)
		}
	}
}

package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	_ "github.com/jackc/pgx/v5/stdlib" // §12.4：不用 lib/pq，驱动名注册为 "pgx"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_PG_DSN")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// warehouseID 按 code 查种子数据（003_seed_warehouses.up.sql）的仓库
// id——不硬编码数字，避免迁移顺序变化时测试跟着假设错位。
func warehouseID(t *testing.T, db *sql.DB, code string) string {
	t.Helper()
	var id int64
	err := besdk.WithTx(context.Background(), db, "erp_inventory_rw", "erp_inventory",
		func(tx *sql.Tx) error {
			return tx.QueryRow(`SELECT id FROM warehouses WHERE code = $1`, code).Scan(&id)
		})
	if err != nil {
		t.Fatalf("查种子仓库 %q 失败（先跑 003_seed_warehouses.up.sql）：%v", code, err)
	}
	return strconv.FormatInt(id, 10)
}

// uniqueProductID 给每个测试造一个独立的 product_id，测试之间不共享
// 余额行，互不干扰（同一张表被多个测试并发跑时尤其重要）。
var productSeq int64

func uniqueProductID(prefix string) string {
	n := atomic.AddInt64(&productSeq, 1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), n)
}

func TestReceive_基本入库(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	pid := uniqueProductID("recv")

	movementID, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-" + pid, ProductID: pid, WarehouseID: east, Qty: "10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if movementID == "" {
		t.Fatal("期望返回非空 movement_id")
	}

	b, err := r.GetBalance(ctx, pid, east)
	if err != nil {
		t.Fatal(err)
	}
	if b.OnHandQty != "10.000000" {
		t.Fatalf("期望 on_hand_qty=10.000000，实际 %q", b.OnHandQty)
	}
}

func TestReceive_幂等(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	pid := uniqueProductID("recv-idem")
	key := "test-recv-idem-" + pid

	id1, err := r.Receive(ctx, ReceiveInput{IdempotencyKey: key, ProductID: pid, WarehouseID: east, Qty: "5"})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := r.Receive(ctx, ReceiveInput{IdempotencyKey: key, ProductID: pid, WarehouseID: east, Qty: "5"})
	if err != nil {
		t.Fatalf("幂等重试报错了：%v", err)
	}
	if id1 != id2 {
		t.Fatalf("幂等失效：第一次 %s，第二次 %s", id1, id2)
	}

	b, err := r.GetBalance(ctx, pid, east)
	if err != nil {
		t.Fatal(err)
	}
	if b.OnHandQty != "5.000000" {
		t.Fatalf("幂等重试不该重复入库，期望 5.000000，实际 %q", b.OnHandQty)
	}
}

func TestGetBalance_没有余额行时返回0而不是报错(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")

	b, err := r.GetBalance(ctx, uniqueProductID("never-received"), east)
	if err != nil {
		t.Fatalf("从没收过货的 (product,warehouse) 应该返回 0 余额而不是报错：%v", err)
	}
	if b.OnHandQty != "0" || b.AvailableQty != "0" {
		t.Fatalf("期望 on_hand/available 都是 0，实际 %+v", b)
	}
}

func TestReserve_库存不足时拒绝且不留痕迹(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	pid := uniqueProductID("insufficient")

	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-" + pid, ProductID: pid, WarehouseID: east, Qty: "3"}); err != nil {
		t.Fatal(err)
	}

	_, err := r.Reserve(ctx, ReserveInput{
		IdempotencyKey: "test-reserve-" + pid, OrderID: "order-1",
		Items: []ReserveItem{{ProductID: pid, WarehouseID: east, Qty: "5"}},
	})
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("库存 3 件预留 5 件应该报 ErrInsufficientStock，实际：%v", err)
	}

	b, err := r.GetBalance(ctx, pid, east)
	if err != nil {
		t.Fatal(err)
	}
	if b.ReservedQty != "0.000000" {
		t.Fatalf("预留失败不该留下任何 reserved_qty，实际 %q", b.ReservedQty)
	}
}

func TestReserve_幂等(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	pid := uniqueProductID("reserve-idem")

	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-" + pid, ProductID: pid, WarehouseID: east, Qty: "10"}); err != nil {
		t.Fatal(err)
	}

	in := ReserveInput{
		IdempotencyKey: "test-reserve-idem-" + pid, OrderID: "order-2",
		Items: []ReserveItem{{ProductID: pid, WarehouseID: east, Qty: "3"}},
	}
	rid1, err := r.Reserve(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	rid2, err := r.Reserve(ctx, in)
	if err != nil {
		t.Fatalf("幂等重试报错了：%v", err)
	}
	if rid1 != rid2 {
		t.Fatalf("幂等失效：第一次 %s，第二次 %s", rid1, rid2)
	}

	b, err := r.GetBalance(ctx, pid, east)
	if err != nil {
		t.Fatal(err)
	}
	if b.ReservedQty != "3.000000" {
		t.Fatalf("幂等重试不该重复预留，期望 reserved_qty=3.000000，实际 %q", b.ReservedQty)
	}
}

func TestReserve_多项其中一项不够时整单回滚(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	pidOK := uniqueProductID("multi-ok")
	pidShort := uniqueProductID("multi-short")

	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-" + pidOK, ProductID: pidOK, WarehouseID: east, Qty: "10"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-" + pidShort, ProductID: pidShort, WarehouseID: east, Qty: "1"}); err != nil {
		t.Fatal(err)
	}

	_, err := r.Reserve(ctx, ReserveInput{
		IdempotencyKey: "test-reserve-multi-" + pidOK, OrderID: "order-3",
		Items: []ReserveItem{
			{ProductID: pidOK, WarehouseID: east, Qty: "5"},    // 这项够
			{ProductID: pidShort, WarehouseID: east, Qty: "5"}, // 这项不够
		},
	})
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("期望 ErrInsufficientStock，实际：%v", err)
	}

	// 第一项即使够，也不该留下预留痕迹——一次 Reserve 调用必须整单原子。
	bOK, err := r.GetBalance(ctx, pidOK, east)
	if err != nil {
		t.Fatal(err)
	}
	if bOK.ReservedQty != "0.000000" {
		t.Fatalf("多项预留里有一项失败，已经成功的那项也必须回滚，实际 reserved_qty=%q", bOK.ReservedQty)
	}
}

func TestCancelReservation_释放后余额恢复且可安全重复调用(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	pid := uniqueProductID("cancel")

	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-" + pid, ProductID: pid, WarehouseID: east, Qty: "10"}); err != nil {
		t.Fatal(err)
	}
	rid, err := r.Reserve(ctx, ReserveInput{
		IdempotencyKey: "test-reserve-" + pid, OrderID: "order-4",
		Items: []ReserveItem{{ProductID: pid, WarehouseID: east, Qty: "4"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	status, err := r.CancelReservation(ctx, CancelReservationInput{
		IdempotencyKey: "test-cancel-" + pid, ReservationID: rid})
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusCancelled {
		t.Fatalf("期望 CANCELLED，实际 %q", status)
	}

	b, err := r.GetBalance(ctx, pid, east)
	if err != nil {
		t.Fatal(err)
	}
	if b.ReservedQty != "0.000000" {
		t.Fatalf("取消预留后 reserved_qty 应该回到 0，实际 %q", b.ReservedQty)
	}

	// 重复调用 Cancel（不同 idempotency_key，模拟"没记住第一次结果的重试"）
	// 必须安全：如实返回 CANCELLED，不报错、不重复扣减。
	status2, err := r.CancelReservation(ctx, CancelReservationInput{
		IdempotencyKey: "test-cancel-retry-" + pid, ReservationID: rid})
	if err != nil {
		t.Fatalf("对一个已经 CANCELLED 的预留重复调用 Cancel 不该报错：%v", err)
	}
	if status2 != StatusCancelled {
		t.Fatalf("期望仍然是 CANCELLED，实际 %q", status2)
	}
}

func TestConfirmIssue_确认后onHand与reserved都扣减且发流水(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	pid := uniqueProductID("confirm")

	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-" + pid, ProductID: pid, WarehouseID: east, Qty: "10"}); err != nil {
		t.Fatal(err)
	}
	rid, err := r.Reserve(ctx, ReserveInput{
		IdempotencyKey: "test-reserve-" + pid, OrderID: "order-5",
		Items: []ReserveItem{{ProductID: pid, WarehouseID: east, Qty: "4"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	status, movementIDs, err := r.ConfirmIssue(ctx, ConfirmIssueInput{
		IdempotencyKey: "test-confirm-" + pid, ReservationID: rid})
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusConfirmed {
		t.Fatalf("期望 CONFIRMED，实际 %q", status)
	}
	if len(movementIDs) != 1 {
		t.Fatalf("一项预留确认应该产生 1 条流水，实际 %d 条", len(movementIDs))
	}

	b, err := r.GetBalance(ctx, pid, east)
	if err != nil {
		t.Fatal(err)
	}
	if b.OnHandQty != "6.000000" || b.ReservedQty != "0.000000" {
		t.Fatalf("确认出库后期望 on_hand=6 reserved=0，实际 on_hand=%q reserved=%q", b.OnHandQty, b.ReservedQty)
	}

	var n int
	if err := besdk.WithTx(ctx, db, "erp_inventory_rw", "erp_inventory", func(tx *sql.Tx) error {
		return tx.QueryRow(
			`SELECT count(*) FROM event_outbox WHERE subject = 'erp.inventory.adjusted.v1' AND aggregate_id = $1`,
			movementIDs[0]).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("期望落 1 条 erp.inventory.adjusted.v1，实际 %d 条", n)
	}
}

// TestGetReservationStatus_区分NotFound与Cancelled 是设计计划 §4.5 那条
// 硬约束的直接测试：合并成一个"没有"是错的。
func TestGetReservationStatus_区分NotFound与Cancelled(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	pid := uniqueProductID("status")

	status, _, err := r.GetReservationStatus(ctx, "999999999", "")
	if err != nil {
		t.Fatalf("查一个从没出现过的 reservation_id 不该报错：%v", err)
	}
	if status != StatusUnspecified {
		t.Fatalf("期望 NOT_FOUND（空字符串），实际 %q", status)
	}

	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-" + pid, ProductID: pid, WarehouseID: east, Qty: "10"}); err != nil {
		t.Fatal(err)
	}
	rid, err := r.Reserve(ctx, ReserveInput{
		IdempotencyKey: "test-reserve-" + pid, OrderID: "order-6",
		Items: []ReserveItem{{ProductID: pid, WarehouseID: east, Qty: "2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CancelReservation(ctx, CancelReservationInput{
		IdempotencyKey: "test-cancel-" + pid, ReservationID: rid}); err != nil {
		t.Fatal(err)
	}

	status, orderID, err := r.GetReservationStatus(ctx, rid, "")
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusCancelled {
		t.Fatalf("已撤销的预留应该查到 CANCELLED，不是 NOT_FOUND，实际 %q", status)
	}
	if orderID != "order-6" {
		t.Fatalf("期望 order_id=order-6，实际 %q", orderID)
	}
}

// TestGetReservationStatus_按idempotencyKey查 是设计计划 §9 那条新增
// 契约字段的直接测试：Reserve 本身超时时调用方拿不到 reservation_id，
// 只能带着当初发的 idempotency_key 查——这条测试验证这条路径查到的
// 结果与按 reservation_id 查完全一致，且查一个从没提交过的
// idempotency_key 会得到真正的 NOT_FOUND（不是报错）。
func TestGetReservationStatus_按idempotencyKey查(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	pid := uniqueProductID("status-idem")
	idemKey := "test-reserve-idem-" + pid

	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-idem-" + pid, ProductID: pid, WarehouseID: east, Qty: "10"}); err != nil {
		t.Fatal(err)
	}
	rid, err := r.Reserve(ctx, ReserveInput{
		IdempotencyKey: idemKey, OrderID: "order-idem-1",
		Items: []ReserveItem{{ProductID: pid, WarehouseID: east, Qty: "3"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 空 reservation_id + 真实 idempotency_key：必须解析出同一个 reservation_id，
	// 状态与直接按 reservation_id 查完全一致。
	statusByKey, orderIDByKey, err := r.GetReservationStatus(ctx, "", idemKey)
	if err != nil {
		t.Fatal(err)
	}
	if statusByKey != StatusReserved {
		t.Fatalf("按 idempotency_key 查期望 RESERVED，实际 %q", statusByKey)
	}
	if orderIDByKey != "order-idem-1" {
		t.Fatalf("期望 order_id=order-idem-1，实际 %q", orderIDByKey)
	}

	// 交叉验证：按 reservation_id 查到的状态与按 idempotency_key 查到的一致
	// ——两条路径必须指向同一行，不会出现"按 id 查是 A、按 key 查是 B"。
	statusByID, orderIDByID, err := r.GetReservationStatus(ctx, rid, "")
	if err != nil {
		t.Fatal(err)
	}
	if statusByID != statusByKey || orderIDByID != orderIDByKey {
		t.Fatalf("两条路径结果不一致：按id=(%q,%q) 按key=(%q,%q)",
			statusByID, orderIDByID, statusByKey, orderIDByKey)
	}

	// 从没提交过的 idempotency_key：真正的 NOT_FOUND，不报错。
	status, orderID, err := r.GetReservationStatus(ctx, "", "从未出现过的-idempotency-key-"+pid)
	if err != nil {
		t.Fatalf("查一个从没提交过的 idempotency_key 不该报错：%v", err)
	}
	if status != StatusUnspecified || orderID != "" {
		t.Fatalf("期望 NOT_FOUND，实际 status=%q order_id=%q", status, orderID)
	}
}

func TestAdjust_不能调到低于已预留量(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	pid := uniqueProductID("adjust-guard")

	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-" + pid, ProductID: pid, WarehouseID: east, Qty: "10"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reserve(ctx, ReserveInput{
		IdempotencyKey: "test-reserve-" + pid, OrderID: "order-7",
		Items: []ReserveItem{{ProductID: pid, WarehouseID: east, Qty: "8"}},
	}); err != nil {
		t.Fatal(err)
	}

	// on_hand=10, reserved=8。盘亏 5 会把 on_hand 调到 5，低于已预留的 8——必须拒绝。
	_, err := r.Adjust(ctx, AdjustInput{
		IdempotencyKey: "test-adjust-" + pid, ProductID: pid, WarehouseID: east,
		QtyDelta: "-5", Reason: "盘点差异",
	})
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("调到低于已预留量应该拒绝，实际：%v", err)
	}
}

func TestAdjust_盘盈盘亏正确记流水与note(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	pid := uniqueProductID("adjust-ok")

	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-" + pid, ProductID: pid, WarehouseID: east, Qty: "10"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Adjust(ctx, AdjustInput{
		IdempotencyKey: "test-adjust-" + pid, ProductID: pid, WarehouseID: east,
		QtyDelta: "-3", Reason: "盘点差异",
	}); err != nil {
		t.Fatal(err)
	}

	b, err := r.GetBalance(ctx, pid, east)
	if err != nil {
		t.Fatal(err)
	}
	if b.OnHandQty != "7.000000" {
		t.Fatalf("期望盘亏后 on_hand=7，实际 %q", b.OnHandQty)
	}

	res, err := r.ListMovements(ctx, ListInput{ProductID: pid, WarehouseID: east, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range res.Movements {
		if m.Reason == "ADJUST_LOSS" && m.Note == "盘点差异" {
			found = true
		}
	}
	if !found {
		t.Fatalf("期望有一条 reason=ADJUST_LOSS note=盘点差异 的流水，实际：%+v", res.Movements)
	}
}

// ── 核心测试：并发防超卖（设计计划 §9 第 1 条，这块砖的核心测试）──
//
// N 个 goroutine 同时预留同一个 SKU，库存只够其中 M 个（M < N）。断言：
// 成功的必须恰好 M 个，余额必须恰好归零，一条超卖都不许有。这条测试挂了
// 就是真的会超卖，不许放宽断言（SOP-W 的 W-5）。
func TestReserve_并发防超卖(t *testing.T) {
	db := testDB(t)
	db.SetMaxOpenConns(50) // 让 N 个并发请求真的用上不同的物理连接，而不是排队串行
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	pid := uniqueProductID("concurrent")

	const stock = 30     // M：库存只够 30 个
	const attempts = 100 // N：100 个并发请求，每个要 1 件

	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-" + pid, ProductID: pid, WarehouseID: east,
		Qty: strconv.Itoa(stock),
	}); err != nil {
		t.Fatal(err)
	}

	var succeeded, failed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := r.Reserve(ctx, ReserveInput{
				IdempotencyKey: fmt.Sprintf("test-concurrent-%s-%d", pid, i),
				OrderID:        fmt.Sprintf("order-concurrent-%d", i),
				Items:          []ReserveItem{{ProductID: pid, WarehouseID: east, Qty: "1"}},
			})
			switch {
			case err == nil:
				succeeded.Add(1)
			case errors.Is(err, ErrInsufficientStock):
				failed.Add(1)
			default:
				t.Errorf("goroutine %d：预期成功或 ErrInsufficientStock，实际是别的错误：%v", i, err)
			}
		}()
	}
	wg.Wait()

	if got := succeeded.Load(); got != stock {
		t.Fatalf("库存 %d 件、%d 个并发请求各要 1 件，期望恰好 %d 个成功，实际成功 %d 个（失败 %d 个）——"+
			"多了就是超卖，少了就是漏判", stock, attempts, stock, got, failed.Load())
	}
	if got := failed.Load(); got != attempts-stock {
		t.Fatalf("期望恰好 %d 个失败，实际 %d 个", attempts-stock, got)
	}

	b, err := r.GetBalance(ctx, pid, east)
	if err != nil {
		t.Fatal(err)
	}
	if b.ReservedQty != fmt.Sprintf("%d.000000", stock) {
		t.Fatalf("期望 reserved_qty 恰好等于库存 %d，实际 %q", stock, b.ReservedQty)
	}
	if b.AvailableQty != "0.000000" {
		t.Fatalf("期望库存恰好被占满，available_qty=0，实际 %q", b.AvailableQty)
	}
}

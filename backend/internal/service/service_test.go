package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-inventory/backend/internal/repo"
	_ "github.com/jackc/pgx/v5/stdlib"
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

func newTestService(t *testing.T) (*Service, *repo.Repo, *sql.DB) {
	t.Helper()
	db := testDB(t)
	r := repo.New(db, "erp_inventory_rw", "erp_inventory")
	return New(r, slog.Default()), r, db
}

func warehouseID(t *testing.T, db *sql.DB, code string) string {
	t.Helper()
	var id int64
	err := besdk.WithTx(context.Background(), db, "erp_inventory_rw", "erp_inventory",
		func(tx *sql.Tx) error {
			return tx.QueryRow(`SELECT id FROM warehouses WHERE code = $1`, code).Scan(&id)
		})
	if err != nil {
		t.Fatalf("查种子仓库 %q 失败：%v", code, err)
	}
	return strconv.FormatInt(id, 10)
}

// authedCtx 造一个"已经过 RequirePermission 验签"的 ctx（besdk.ContextWithClaims，
// 阶段三 Task 6 发现的真实缺口，见 be-sdk-go authz.go 同名函数注释），
// 并真的把 sub 授权到 warehouseIDs——service 层调用 GetBalance/Receive/
// Adjust/ListMovements 时会真的查 warehouse_access 表，只造一份假 Claims
// 不授权访问，一样会被 ErrForbidden 拦下来，测的就不是"输入校验"这件事了。
func authedCtx(t *testing.T, r *repo.Repo, sub string, warehouseIDs ...string) context.Context {
	t.Helper()
	ctx := context.Background()
	for _, whID := range warehouseIDs {
		if err := r.GrantWarehouseAccess(ctx, sub, whID); err != nil {
			t.Fatalf("授权仓库访问失败：%v", err)
		}
	}
	return besdk.ContextWithClaims(ctx, besdk.Claims{Sub: sub})
}

var svcProductSeq int64

func svcUniqueProductID(prefix string) string {
	n := atomic.AddInt64(&svcProductSeq, 1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), n)
}

func TestReserve_idempotencyKey为空时拒绝且不落库(t *testing.T) {
	svc, _, db := newTestService(t)
	ctx := context.Background()
	east := warehouseID(t, db, "WH-EAST")
	pid := svcUniqueProductID("svc-l3-idem-empty")

	_, err := svc.Reserve(ctx, repo.ReserveInput{
		Items: []repo.ReserveItem{{ProductID: pid, WarehouseID: east, Qty: "1"}},
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("idempotency_key 为空应该拒绝，实际：%v", err)
	}
}

func TestReserve_items为空时拒绝(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.Reserve(ctx, repo.ReserveInput{IdempotencyKey: "svc-l3-items-empty"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("items 为空应该拒绝，实际：%v", err)
	}
}

func TestReserve_qty为0或负数时拒绝(t *testing.T) {
	svc, _, db := newTestService(t)
	ctx := context.Background()
	east := warehouseID(t, db, "WH-EAST")
	pid := svcUniqueProductID("svc-l3-qty-bad")

	for _, qty := range []string{"0", "-1", ""} {
		_, err := svc.Reserve(ctx, repo.ReserveInput{
			IdempotencyKey: "svc-l3-qty-" + qty + "-" + pid,
			Items:          []repo.ReserveItem{{ProductID: pid, WarehouseID: east, Qty: qty}},
		})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("qty=%q 应该拒绝，实际：%v", qty, err)
		}
	}
}

func TestReceive_qty为负数时拒绝(t *testing.T) {
	svc, _, db := newTestService(t)
	ctx := context.Background()
	east := warehouseID(t, db, "WH-EAST")
	pid := svcUniqueProductID("svc-l3-recv-neg")

	_, err := svc.Receive(ctx, repo.ReceiveInput{
		IdempotencyKey: "svc-l3-recv-neg-" + pid, ProductID: pid, WarehouseID: east, Qty: "-5",
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("入库数量为负数应该拒绝，实际：%v", err)
	}
}

func TestAdjust_qtyDelta为0时拒绝(t *testing.T) {
	svc, _, db := newTestService(t)
	ctx := context.Background()
	east := warehouseID(t, db, "WH-EAST")
	pid := svcUniqueProductID("svc-l3-adjust-zero")

	_, err := svc.Adjust(ctx, repo.AdjustInput{
		IdempotencyKey: "svc-l3-adjust-zero-" + pid, ProductID: pid, WarehouseID: east,
		QtyDelta: "0", Reason: "test",
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("qty_delta=0 应该拒绝（调整为 0 没有意义），实际：%v", err)
	}
}

// TestAdjust_负数是合法输入 是上一条的边界对照组：Adjust 允许负数
// （盘亏），只有 0 和非法格式才拒绝。
func TestAdjust_负数是合法输入(t *testing.T) {
	svc, r, db := newTestService(t)
	east := warehouseID(t, db, "WH-EAST")
	pid := svcUniqueProductID("svc-l3-adjust-neg-ok")
	ctx := authedCtx(t, r, "u_test-adjust-neg-ok", east)

	if _, err := svc.Receive(ctx, repo.ReceiveInput{
		IdempotencyKey: "svc-recv-" + pid, ProductID: pid, WarehouseID: east, Qty: "10"}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Adjust(ctx, repo.AdjustInput{
		IdempotencyKey: "svc-l3-adjust-neg-ok-" + pid, ProductID: pid, WarehouseID: east,
		QtyDelta: "-2", Reason: "盘亏",
	})
	if err != nil {
		t.Fatalf("盘亏（负数 qty_delta）应该允许，实际报错：%v", err)
	}
}

func TestGetReservationStatus_两个都为空时拒绝(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	_, _, _, err := svc.GetReservationStatus(ctx, "", "")
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("reservation_id 与 idempotency_key 都为空应该拒绝，实际：%v", err)
	}
}

// TestGetReservationStatus_查不到不是InvalidArgument 判据来自设计计划
// §4.5：NOT_FOUND 是正常业务结果，不该被 service 层的校验拦下来当成
// 错误——只有"格式不对"（比如非数字）才是校验该管的范围。
func TestGetReservationStatus_查不到不是错误(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	status, _, _, err := svc.GetReservationStatus(ctx, "999999999999", "")
	if err != nil {
		t.Fatalf("查一个不存在的 reservation_id 不该报错：%v", err)
	}
	if status != repo.StatusUnspecified {
		t.Fatalf("期望 NOT_FOUND，实际 %q", status)
	}
}

package service

import (
	"context"
	"database/sql"
	"log/slog"
	"sort"
	"strconv"
	"testing"

	besdk "github.com/brickKit/be-sdk-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/brickKit/erp-inventory/v2/backend/internal/repo"
)

// 这三个读端点（仓库列表、余额列表、库存统计）都按 warehouse 维过滤：调用者
// 只能看到 warehouse_access 里授给自己的仓库。测试库里别的测试也往种子仓库
// WH-EAST / WH-SOUTH 写余额，所以这里每个测试新建自己的两个仓库，统计数才是
// 确定的。

// newWarehouse 在测试库里新建一个仓库（仓库没有管理接口，测试直接写表）。
func newWarehouse(t *testing.T, db *sql.DB, prefix string) string {
	t.Helper()
	code := svcUniqueProductID(prefix)
	var id int64
	err := besdk.WithTx(context.Background(), db, "erp_inventory_rw", "erp_inventory", func(tx *sql.Tx) error {
		return tx.QueryRow(`INSERT INTO warehouses (code, name) VALUES ($1, $2) RETURNING id`,
			code, "测试仓 "+code).Scan(&id)
	})
	if err != nil {
		t.Fatalf("建测试仓库：%v", err)
	}
	return strconv.FormatInt(id, 10)
}

func receive(t *testing.T, r *repo.Repo, productID, warehouseID, qty string) {
	t.Helper()
	whID, _ := strconv.ParseInt(warehouseID, 10, 64)
	if _, err := r.Receive(context.Background(), repo.ReceiveInput{
		IdempotencyKey: "stats-recv-" + productID + "-" + warehouseID, ProductID: productID,
		WarehouseID: warehouseID, Qty: qty, AllowedWarehouseIDs: []int64{whID},
	}); err != nil {
		t.Fatalf("入库 %s@%s：%v", productID, warehouseID, err)
	}
}

func reserve(t *testing.T, r *repo.Repo, productID, warehouseID, qty string) {
	t.Helper()
	if _, err := r.Reserve(context.Background(), repo.ReserveInput{
		IdempotencyKey: "stats-reserve-" + productID + "-" + warehouseID, OrderID: "order-stats",
		Items: []repo.ReserveItem{{ProductID: productID, WarehouseID: warehouseID, Qty: qty}},
	}); err != nil {
		t.Fatalf("预留 %s@%s：%v", productID, warehouseID, err)
	}
}

// twoWarehouses 建两个仓库、两个用户：A 只授权仓 a，B 只授权仓 b。
type twoWarehouses struct {
	a, b       string
	ctxA, ctxB context.Context
}

func setupTwoWarehouses(t *testing.T, r *repo.Repo, db *sql.DB, prefix string) twoWarehouses {
	t.Helper()
	w := twoWarehouses{a: newWarehouse(t, db, prefix+"-a"), b: newWarehouse(t, db, prefix+"-b")}
	w.ctxA = authedCtx(t, r, svcUniqueProductID(prefix+"-userA"), w.a)
	w.ctxB = authedCtx(t, r, svcUniqueProductID(prefix+"-userB"), w.b)
	return w
}

func TestListWarehouses_只返回自己有授权的仓库别人的看不到(t *testing.T) {
	svc, r, db := newTestService(t)
	w := setupTwoWarehouses(t, r, db, "wh-list")

	for _, c := range []struct {
		name      string
		ctx       context.Context
		want, not string
	}{{"A", w.ctxA, w.a, w.b}, {"B", w.ctxB, w.b, w.a}} {
		got, err := svc.ListWarehouses(c.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != c.want {
			t.Fatalf("用户 %s 只授权了仓库 %s，应恰好看到它，实际 %+v", c.name, c.want, got)
		}
		if got[0].Code == "" || got[0].Name == "" || got[0].Status != "ACTIVE" {
			t.Fatalf("仓库应带 code / name / status，实际 %+v", got[0])
		}
		for _, wh := range got {
			if wh.ID == c.not {
				t.Fatalf("用户 %s 看到了没授权的仓库 %s", c.name, c.not)
			}
		}
	}

	nobody := authedCtx(t, r, svcUniqueProductID("wh-list-nobody"))
	got, err := svc.ListWarehouses(nobody)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("没有任何授权的用户应看不到任何仓库，实际 %+v", got)
	}
}

func TestListBalances_只看到授权仓库的余额且游标翻页不重不漏(t *testing.T) {
	svc, r, db := newTestService(t)
	w := setupTwoWarehouses(t, r, db, "bal-list")
	var wantA []string
	for i := 0; i < 3; i++ {
		pid := svcUniqueProductID("bal-list-a")
		receive(t, r, pid, w.a, "5")
		wantA = append(wantA, pid)
	}
	pidB := svcUniqueProductID("bal-list-b")
	receive(t, r, pidB, w.b, "7")

	var got []string
	cursor := ""
	for page := 0; page < 10; page++ {
		res, err := svc.ListBalances(w.ctxA, repo.BalanceListInput{PageSize: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Balances) > 2 {
			t.Fatalf("一页最多 2 条，实际 %d 条", len(res.Balances))
		}
		for _, b := range res.Balances {
			if b.WarehouseID != w.a {
				t.Fatalf("用户 A 看到了别的仓库（%s）的余额：%+v", b.WarehouseID, b)
			}
			if b.OnHandQty == "" || b.AvailableQty == "" {
				t.Fatalf("余额应带 on_hand_qty / available_qty：%+v", b)
			}
			got = append(got, b.ProductID)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	sort.Strings(got)
	sort.Strings(wantA)
	if len(got) != len(wantA) {
		t.Fatalf("用户 A 应恰好看到仓库 a 的 %d 行余额（不重不漏），实际 %v", len(wantA), got)
	}
	for i := range got {
		if got[i] != wantA[i] {
			t.Fatalf("用户 A 看到的余额 %v，期望 %v", got, wantA)
		}
	}

	// 按产品过滤；点名一个没授权的仓库时结果为空，不泄露别人的余额。
	res, err := svc.ListBalances(w.ctxA, repo.BalanceListInput{ProductID: wantA[1]})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Balances) != 1 || res.Balances[0].ProductID != wantA[1] {
		t.Fatalf("按 product_id=%s 过滤应只剩 1 行，实际 %+v", wantA[1], res.Balances)
	}
	res, err = svc.ListBalances(w.ctxA, repo.BalanceListInput{WarehouseID: w.b})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Balances) != 0 {
		t.Fatalf("用户 A 点名没授权的仓库 b，应看不到任何余额，实际 %+v", res.Balances)
	}
	res, err = svc.ListBalances(w.ctxB, repo.BalanceListInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Balances) != 1 || res.Balances[0].ProductID != pidB {
		t.Fatalf("用户 B 应只看到仓库 b 的 1 行余额，实际 %+v", res.Balances)
	}
}

func TestListBalances_非法游标是入参错误(t *testing.T) {
	svc, r, db := newTestService(t)
	w := setupTwoWarehouses(t, r, db, "bal-bad-cursor")
	_, err := svc.ListBalances(w.ctxA, repo.BalanceListInput{Cursor: "!!!"})
	if err == nil {
		t.Fatal("非法游标应被拒绝")
	}
	if got := status.Code(ToStatus(err)); got != codes.InvalidArgument {
		t.Fatalf("非法游标应映射成 InvalidArgument，实际 %v（%v）", got, err)
	}
}

// TestStockSummary_按授权仓库统计别人的仓库看不到：仓 a 有三个产品——在手 50
// （充足）、在手 3（低于阈值）、在手 20 但预留 15（在途预留让可用量只剩 5，
// 低于阈值）；仓 b 有一个在手 1000 的产品。默认阈值 10 下，A 的统计只算仓 a。
func TestStockSummary_按授权仓库统计别人的仓库看不到(t *testing.T) {
	svc, r, db := newTestService(t)
	w := setupTwoWarehouses(t, r, db, "stats")
	plenty := svcUniqueProductID("stats-plenty")
	low := svcUniqueProductID("stats-low")
	reserved := svcUniqueProductID("stats-reserved")
	receive(t, r, plenty, w.a, "50")
	receive(t, r, low, w.a, "3")
	receive(t, r, reserved, w.a, "20")
	reserve(t, r, reserved, w.a, "15")
	other := svcUniqueProductID("stats-other")
	receive(t, r, other, w.b, "1000")

	sum, err := svc.StockSummary(w.ctxA)
	if err != nil {
		t.Fatal(err)
	}
	if sum.SKUCount != 3 {
		t.Fatalf("用户 A 的 sku_count 应为 3（只算仓 a），实际 %d", sum.SKUCount)
	}
	if !decimalEqual(t, sum.TotalOnHandQty, "73") {
		t.Fatalf("用户 A 的 total_on_hand_qty 应为 73（50+3+20，不含仓 b 的 1000），实际 %q", sum.TotalOnHandQty)
	}
	gotLow := map[string]string{}
	for _, it := range sum.LowStock {
		if it.WarehouseID != w.a {
			t.Fatalf("低库存清单里出现了别的仓库：%+v", it)
		}
		gotLow[it.ProductID] = it.AvailableQty
	}
	if len(gotLow) != 2 || !decimalEqual(t, gotLow[low], "3") || !decimalEqual(t, gotLow[reserved], "5") {
		t.Fatalf("阈值 10 下低库存应是 %s（可用 3）与 %s（可用 5，在途预留 15），实际 %+v", low, reserved, sum.LowStock)
	}
	if sum.LowStockCount != 2 {
		t.Fatalf("low_stock_count 应为 2，实际 %d", sum.LowStockCount)
	}
	if sum.LowStock[0].ProductID != low {
		t.Fatalf("低库存清单应按可用量升序（%s 的 3 在前），实际 %+v", low, sum.LowStock)
	}

	sumB, err := svc.StockSummary(w.ctxB)
	if err != nil {
		t.Fatal(err)
	}
	if sumB.SKUCount != 1 || !decimalEqual(t, sumB.TotalOnHandQty, "1000") || len(sumB.LowStock) != 0 {
		t.Fatalf("用户 B 只应看到仓 b：sku_count=1、total=1000、无低库存，实际 %+v", sumB)
	}

	nobody := authedCtx(t, r, svcUniqueProductID("stats-nobody"))
	sumN, err := svc.StockSummary(nobody)
	if err != nil {
		t.Fatal(err)
	}
	if sumN.SKUCount != 0 || !decimalEqual(t, sumN.TotalOnHandQty, "0") || len(sumN.LowStock) != 0 || sumN.LowStockCount != 0 {
		t.Fatalf("没有任何授权的用户应看到全零的统计，实际 %+v", sumN)
	}
}

// TestStockSummary_阈值配成非默认值时低库存清单随之变化：同一份数据，阈值 10
// 时只有可用 3 的那一行；阈值 60 时可用 50 的那一行也算低库存。阈值是十进制
// 比较："4.5" 时可用 3 算、可用 5 不算。
func TestStockSummary_阈值配成非默认值时低库存清单随之变化(t *testing.T) {
	_, r, db := newTestService(t)
	w := setupTwoWarehouses(t, r, db, "stats-threshold")
	p3 := svcUniqueProductID("th-3")
	p5 := svcUniqueProductID("th-5")
	p50 := svcUniqueProductID("th-50")
	receive(t, r, p3, w.a, "3")
	receive(t, r, p5, w.a, "5")
	receive(t, r, p50, w.a, "50")

	lowSet := func(threshold string) []string {
		svc := New(r, slog.Default(), WithLowStockThreshold(threshold))
		sum, err := svc.StockSummary(w.ctxA)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range sum.LowStock {
			out = append(out, it.ProductID)
		}
		sort.Strings(out)
		return out
	}
	sorted := func(ids ...string) []string { sort.Strings(ids); return ids }

	for _, c := range []struct {
		threshold string
		want      []string
	}{
		{"10", sorted(p3, p5)},
		{"60", sorted(p3, p5, p50)},
		{"4.5", sorted(p3)},
		{"3", nil},
	} {
		got := lowSet(c.threshold)
		if len(got) != len(c.want) {
			t.Fatalf("阈值 %s 时低库存应为 %v，实际 %v", c.threshold, c.want, got)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("阈值 %s 时低库存应为 %v，实际 %v", c.threshold, c.want, got)
			}
		}
	}
}

func TestValidateLowStockThreshold_只接受非负十进制数(t *testing.T) {
	for _, ok := range []string{"0", "10", "4.5", "1000000"} {
		if err := ValidateLowStockThreshold(ok); err != nil {
			t.Fatalf("%q 是合法阈值，却被拒绝：%v", ok, err)
		}
	}
	for _, bad := range []string{"", "abc", "-1", "1e3", "10 ", ".5", "5."} {
		if err := ValidateLowStockThreshold(bad); err == nil {
			t.Fatalf("%q 不是合法的非负十进制数，应被拒绝", bad)
		}
	}
}

// decimalEqual 用数据库比较两个十进制字符串（"73" 与 "73.000000" 相等），不经过浮点数。
func decimalEqual(t *testing.T, a, b string) bool {
	t.Helper()
	if a == "" || b == "" {
		return a == b
	}
	db := testDB(t)
	var eq bool
	if err := db.QueryRow(`SELECT $1::numeric = $2::numeric`, a, b).Scan(&eq); err != nil {
		t.Fatalf("比较 %q 与 %q：%v", a, b, err)
	}
	return eq
}

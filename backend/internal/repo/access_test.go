// 阶段三 Task 6：warehouse_access 分配表 + 数据范围过滤真实生效——
// 对应 004_create_warehouse_access.up.sql 顶部注释。
package repo

import (
	"context"
	"errors"
	"testing"
)

func TestWarehouseAccess_grant与revoke(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	south := warehouseID(t, db, "WH-SOUTH")
	sub := uniqueProductID("sub-grant") // 复用它造唯一字符串，不是产品 id 语义

	ids, err := r.WarehouseIDsFor(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("从没授权过应该是空列表，实际 %v", ids)
	}

	if err := r.GrantWarehouseAccess(ctx, sub, east); err != nil {
		t.Fatal(err)
	}
	ids, err = r.WarehouseIDsFor(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("授权一个仓库后应该只有一条，实际 %v", ids)
	}

	// 重复授权同一个仓库——幂等，不报错、不产生第二条。
	if err := r.GrantWarehouseAccess(ctx, sub, east); err != nil {
		t.Fatalf("重复授权不该报错：%v", err)
	}
	ids, _ = r.WarehouseIDsFor(ctx, sub)
	if len(ids) != 1 {
		t.Fatalf("重复授权同一个仓库不该产生第二条，实际 %v", ids)
	}

	if err := r.GrantWarehouseAccess(ctx, sub, south); err != nil {
		t.Fatal(err)
	}
	ids, _ = r.WarehouseIDsFor(ctx, sub)
	if len(ids) != 2 {
		t.Fatalf("授权两个仓库后应该有两条，实际 %v", ids)
	}

	if err := r.RevokeWarehouseAccess(ctx, sub, east); err != nil {
		t.Fatal(err)
	}
	ids, _ = r.WarehouseIDsFor(ctx, sub)
	if len(ids) != 1 || allowedWarehouses(t, south)[0] != ids[0] {
		t.Fatalf("撤销东仓后应该只剩南仓，实际 %v", ids)
	}

	// 撤销一条本来就不存在的分配——幂等，不报错。
	if err := r.RevokeWarehouseAccess(ctx, sub, east); err != nil {
		t.Fatalf("撤销一条不存在的分配不该报错：%v", err)
	}
}

func TestWarehouseAccess_授权不存在的仓库拒绝(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	sub := uniqueProductID("sub-badwh")

	err := r.GrantWarehouseAccess(ctx, sub, "999999999")
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("授权一个不存在的仓库 id 应该是 ErrInvalidArgument，实际：%v", err)
	}
}

// TestGetBalance_没有授权时ErrForbidden 是 warehouse 维数据权限的核心
// 断言：点名一个真实存在、但没有被授权访问的仓库，必须是 ErrForbidden，
// 不能是"库存为 0"——那两种情况对调用者的含义完全不同（见 ErrForbidden
// 文档注释）。
func TestGetBalance_没有授权时ErrForbidden(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	south := warehouseID(t, db, "WH-SOUTH")
	pid := uniqueProductID("forbidden-balance")

	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-" + pid, ProductID: pid, WarehouseID: east,
		AllowedWarehouseIDs: allowedWarehouses(t, east), Qty: "10",
	}); err != nil {
		t.Fatal(err)
	}

	// 只被授权南仓，查东仓的余额——即使东仓真的有货，也必须是 ErrForbidden。
	_, err := r.GetBalance(ctx, pid, east, allowedWarehouses(t, south))
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("没有授权的仓库应该是 ErrForbidden，实际：%v", err)
	}
}

// TestReceive_没有授权时ErrForbidden 覆盖写路径：不能靠"只保护读接口"
// 就以为 warehouse 维数据权限生效了——如果写接口不检查，调用方随便点一个
// 仓库 id 就能往里面写数据，读接口的保护形同虚设。
func TestReceive_没有授权时ErrForbidden(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	south := warehouseID(t, db, "WH-SOUTH")
	pid := uniqueProductID("forbidden-receive")

	_, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-forbidden-" + pid, ProductID: pid, WarehouseID: east,
		AllowedWarehouseIDs: allowedWarehouses(t, south), // 只授权了南仓，却往东仓入库
		Qty: "10",
	})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("对没有授权的仓库入库应该是 ErrForbidden，实际：%v", err)
	}

	// 且这次被拒绝的入库真的没有落地——不是"报错了但其实还是写进去了"。
	b, err := r.GetBalance(ctx, pid, east, allowedWarehouses(t, east))
	if err != nil {
		t.Fatal(err)
	}
	if b.OnHandQty != "0" {
		t.Fatalf("被拒绝的入库不该有任何余额落地，实际 on_hand=%q", b.OnHandQty)
	}
}

// TestListMovements_只看到授权仓库的流水 是 List 端点数据范围过滤的核心
// 断言：授权范围必须下推进 SQL 的 WHERE，不能查出全部结果后在 Go 里再
// 过滤那一半（决策 53 的既有判据同样适用于这里）。
func TestListMovements_只看到授权仓库的流水(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	south := warehouseID(t, db, "WH-SOUTH")
	pid := uniqueProductID("scope-list")

	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-east-" + pid, ProductID: pid, WarehouseID: east,
		AllowedWarehouseIDs: allowedWarehouses(t, east), Qty: "5",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-south-" + pid, ProductID: pid, WarehouseID: south,
		AllowedWarehouseIDs: allowedWarehouses(t, south), Qty: "7",
	}); err != nil {
		t.Fatal(err)
	}

	// 只授权东仓——即使南仓真的有这个 product 的流水，也不该出现在结果里。
	res, err := r.ListMovements(ctx, ListInput{
		ProductID: pid, PageSize: 10, AllowedWarehouseIDs: allowedWarehouses(t, east),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Movements) != 1 {
		t.Fatalf("只授权东仓，应该只看到 1 条流水，实际 %d 条：%+v", len(res.Movements), res.Movements)
	}
	if res.Movements[0].WarehouseID != east {
		t.Fatalf("看到的流水应该是东仓的，实际 warehouse_id=%q", res.Movements[0].WarehouseID)
	}

	// 授权两个仓库——两条流水都该看到。
	res, err = r.ListMovements(ctx, ListInput{
		ProductID: pid, PageSize: 10, AllowedWarehouseIDs: allowedWarehouses(t, east, south),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Movements) != 2 {
		t.Fatalf("授权两个仓库后应该看到 2 条流水，实际 %d 条", len(res.Movements))
	}
}

// TestListMovements_没有任何授权时看不到任何流水 是 fail-closed 方向的
// 直接验证：一条分配都没有 = 谁都看不见，不是"恒不限"。
func TestListMovements_没有任何授权时看不到任何流水(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	pid := uniqueProductID("scope-empty")

	if _, err := r.Receive(ctx, ReceiveInput{
		IdempotencyKey: "test-recv-" + pid, ProductID: pid, WarehouseID: east,
		AllowedWarehouseIDs: allowedWarehouses(t, east), Qty: "5",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := r.ListMovements(ctx, ListInput{ProductID: pid, PageSize: 10, AllowedWarehouseIDs: nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Movements) != 0 {
		t.Fatalf("没有任何授权应该一条都看不见，实际 %d 条", len(res.Movements))
	}
}

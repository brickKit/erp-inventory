package repo

import (
	"context"
	"strconv"
	"testing"
)

// TestListMovements_游标翻页不重不漏且可按仓库过滤：同一个产品在两个仓库各入库
// 若干次，按 page_size=2 一页页翻，拿到的流水恰好是全部、没有重复，并且按
// (created_at, id) 倒序；再加 warehouse_id 过滤，只剩那个仓库的。守的是列表
// 查询的分页与过滤语义，不随 SQL 的写法变化。
func TestListMovements_游标翻页不重不漏且可按仓库过滤(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_inventory_rw", "erp_inventory")
	east := warehouseID(t, db, "WH-EAST")
	south := warehouseID(t, db, "WH-SOUTH")
	allowed := allowedWarehouses(t, east, south)
	pid := uniqueProductID("list-page")

	want := map[string]string{} // movement_id → warehouse_id
	for i := 0; i < 5; i++ {
		wh := east
		if i%2 == 1 {
			wh = south
		}
		id, err := r.Receive(ctx, ReceiveInput{
			IdempotencyKey: "list-page-" + pid + "-" + strconv.Itoa(i), ProductID: pid,
			WarehouseID: wh, Qty: "1", AllowedWarehouseIDs: allowed,
		})
		if err != nil {
			t.Fatal(err)
		}
		want[id] = wh
	}

	seen := map[string]bool{}
	var order []string
	cursor := ""
	for page := 0; page < 10; page++ {
		res, err := r.ListMovements(ctx, ListInput{
			ProductID: pid, PageSize: 2, Cursor: cursor, AllowedWarehouseIDs: allowed,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Movements) > 2 {
			t.Fatalf("一页最多 2 条，实际 %d 条", len(res.Movements))
		}
		for _, m := range res.Movements {
			if seen[m.ID] {
				t.Fatalf("流水 %s 在翻页中出现了两次", m.ID)
			}
			seen[m.ID] = true
			order = append(order, m.ID)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	if len(seen) != len(want) {
		t.Fatalf("翻完应该恰好拿到 %d 条流水，实际 %d 条（%v）", len(want), len(seen), order)
	}
	for i := 1; i < len(order); i++ {
		a, _ := strconv.ParseInt(order[i-1], 10, 64)
		b, _ := strconv.ParseInt(order[i], 10, 64)
		if a < b {
			t.Fatalf("流水应按时间倒序（同一时刻按 id 倒序），实际顺序 %v", order)
		}
	}

	res, err := r.ListMovements(ctx, ListInput{
		ProductID: pid, WarehouseID: south, PageSize: 10, AllowedWarehouseIDs: allowed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Movements) != 2 {
		t.Fatalf("按 warehouse_id=%s 过滤应该只剩 2 条，实际 %d 条", south, len(res.Movements))
	}
	for _, m := range res.Movements {
		if m.WarehouseID != south {
			t.Fatalf("过滤后出现了别的仓库的流水：%+v", m)
		}
	}
}

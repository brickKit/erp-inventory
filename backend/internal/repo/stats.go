package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	besdk "github.com/brickKit/be-sdk-go"
)

// LowStockItem 是低库存清单的一项。
type LowStockItem struct {
	ProductID    string
	WarehouseID  string
	AvailableQty string
}

// StockSummary 是仪表盘的库存统计，只算调用者有授权的仓库。
type StockSummary struct {
	SKUCount       int64  // 在手量 > 0 的不同产品数
	TotalOnHandQty string // 在手量合计，十进制字符串
	LowStock       []*LowStockItem
	LowStockCount  int64 // 满足低库存条件的总行数（LowStock 只列前 limit 条）
}

// StockSummary 统计 allowedWarehouseIDs 里的余额。低库存 = 可用量（在手 -
// 已预留）< threshold；threshold 是十进制字符串，在 SQL 里按 numeric 比较，
// 全程不经过浮点数。低库存清单按可用量升序，最多 limit 条。
func (r *Repo) StockSummary(ctx context.Context, allowedWarehouseIDs []int64, threshold string, limit int) (*StockSummary, error) {
	if allowedWarehouseIDs == nil {
		allowedWarehouseIDs = []int64{}
	}
	out := &StockSummary{LowStock: []*LowStockItem{}}
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			SELECT count(DISTINCT product_id) FILTER (WHERE on_hand_qty > 0),
			       COALESCE(sum(on_hand_qty), 0),
			       count(*) FILTER (WHERE on_hand_qty - reserved_qty < $2::numeric)
			  FROM inventory_balances
			 WHERE warehouse_id = ANY($1::bigint[])`,
			allowedWarehouseIDs, threshold,
		).Scan(&out.SKUCount, &out.TotalOnHandQty, &out.LowStockCount); err != nil {
			return fmt.Errorf("统计 inventory_balances: %w", err)
		}

		rows, err := tx.QueryContext(ctx, `
			SELECT product_id, warehouse_id, on_hand_qty - reserved_qty
			  FROM inventory_balances
			 WHERE warehouse_id = ANY($1::bigint[])
			   AND on_hand_qty - reserved_qty < $2::numeric
			 ORDER BY on_hand_qty - reserved_qty, product_id, warehouse_id
			 LIMIT $3`,
			allowedWarehouseIDs, threshold, limit)
		if err != nil {
			return fmt.Errorf("查低库存: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var it LowStockItem
			var rawWarehouseID int64
			if err := rows.Scan(&it.ProductID, &rawWarehouseID, &it.AvailableQty); err != nil {
				return err
			}
			it.WarehouseID = strconv.FormatInt(rawWarehouseID, 10)
			out.LowStock = append(out.LowStock, &it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

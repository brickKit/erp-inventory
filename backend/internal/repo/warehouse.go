package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	besdk "github.com/brickKit/be-sdk-go"
)

// Warehouse 是 warehouses 一行的视图。
type Warehouse struct {
	ID     string
	Code   string
	Name   string
	Status string
}

// ListWarehouses 返回 allowedWarehouseIDs 里的仓库（调用者的 warehouse_access
// 授权），按 id 升序。仓库是迁移播种的主数据，几十行量级，不分页。空列表 =
// 谁都看不见（= ANY('{}') 天然匹配不到）。
func (r *Repo) ListWarehouses(ctx context.Context, allowedWarehouseIDs []int64) ([]*Warehouse, error) {
	if allowedWarehouseIDs == nil {
		allowedWarehouseIDs = []int64{}
	}
	out := []*Warehouse{}
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, code, name, status FROM warehouses
			 WHERE id = ANY($1::bigint[])
			 ORDER BY id`, allowedWarehouseIDs)
		if err != nil {
			return fmt.Errorf("查 warehouses: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var w Warehouse
			var rawID int64
			if err := rows.Scan(&rawID, &w.Code, &w.Name, &w.Status); err != nil {
				return err
			}
			w.ID = strconv.FormatInt(rawID, 10)
			out = append(out, &w)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

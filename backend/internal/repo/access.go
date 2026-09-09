// warehouse_access——阶段三 Task 6 的 warehouse 维数据权限分配表（见
// 004_create_warehouse_access.up.sql 顶部注释）。这三个函数是本组件
// 唯一读写这张表的入口，被 service 层的读接口（过滤 List/Get）与
// 管理接口（grant/revoke）共用。
package repo

import (
	"context"
	"database/sql"
	"fmt"

	besdk "github.com/brickKit/be-sdk-go"
)

// WarehouseIDsFor 查 sub 当前能访问的全部仓库 id——List/Get 的过滤条件、
// 管理接口的"查看当前分配"共用同一条查询。没有任何分配时返回空切片
// （不是 nil，调用方按"空列表=谁都看不见"处理，纯 SQL `= ANY('{}')`
// 天然实现，不需要特判）。
func (r *Repo) WarehouseIDsFor(ctx context.Context, sub string) ([]int64, error) {
	var ids []int64
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT warehouse_id FROM warehouse_access WHERE sub = $1 ORDER BY warehouse_id`, sub)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("查 warehouse_access: %w", err)
	}
	if ids == nil {
		ids = []int64{}
	}
	return ids, nil
}

// GrantWarehouseAccess 幂等授予——重复授予不报错（同 infra-authz
// GrantUserRole 的既有约定）。warehouseID 不存在时把外键冲突翻成
// ErrInvalidArgument，不是内部错误。
func (r *Repo) GrantWarehouseAccess(ctx context.Context, sub, warehouseID string) error {
	whID, err := parseWarehouseID(warehouseID)
	if err != nil {
		return err
	}
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO warehouse_access (sub, warehouse_id) VALUES ($1, $2)
			ON CONFLICT (sub, warehouse_id) DO NOTHING`, sub, whID)
		return err
	})
	if err != nil {
		if isForeignKeyViolation(err) {
			return ErrInvalidArgument
		}
		return fmt.Errorf("授予仓库访问权限: %w", err)
	}
	return nil
}

// RevokeWarehouseAccess 幂等撤销——撤销一条本来就不存在的分配不报错，
// 同 grant 的对称约定。
func (r *Repo) RevokeWarehouseAccess(ctx context.Context, sub, warehouseID string) error {
	whID, err := parseWarehouseID(warehouseID)
	if err != nil {
		return err
	}
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM warehouse_access WHERE sub = $1 AND warehouse_id = $2`, sub, whID)
		return err
	})
	if err != nil {
		return fmt.Errorf("撤销仓库访问权限: %w", err)
	}
	return nil
}

package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	besdk "github.com/brickKit/be-sdk-go"
)

// Balance 是 inventory_balances 一行的视图。AvailableQty 由服务端算好直接给，
// 不让调用方自己减。数量一律是十进制字符串。
type Balance struct {
	ProductID    string
	WarehouseID  string
	OnHandQty    string
	ReservedQty  string
	AvailableQty string
	Version      int64
}

func scanBalanceRow(row rowScanner) (*Balance, error) {
	var b Balance
	var rawWarehouseID int64
	if err := row.Scan(&b.ProductID, &rawWarehouseID, &b.OnHandQty, &b.ReservedQty,
		&b.AvailableQty, &b.Version); err != nil {
		return nil, err
	}
	b.WarehouseID = strconv.FormatInt(rawWarehouseID, 10)
	return &b, nil
}

// GetBalance：查不到余额行等价于库存为 0（从没收过货），不是错误——一个从没
// 进过货的 (product, warehouse) 组合问"还有多少"，答案就是 0。
//
// allowedWarehouseIDs 是调用者的 warehouse_access 授权列表：点名的
// warehouseID 不在里面就是 ErrForbidden，不是"库存为 0"——两种情况对调用者
// 的含义完全不同。
func (r *Repo) GetBalance(ctx context.Context, productID, warehouseID string, allowedWarehouseIDs []int64) (*Balance, error) {
	whID, err := parseWarehouseID(warehouseID)
	if err != nil {
		return nil, err
	}
	if !containsInt64(allowedWarehouseIDs, whID) {
		return nil, ErrForbidden
	}
	var b Balance
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			SELECT product_id, warehouse_id, on_hand_qty, reserved_qty,
				on_hand_qty - reserved_qty, version
			FROM inventory_balances WHERE product_id = $1 AND warehouse_id = $2`,
			productID, whID)
		got, err := scanBalanceRow(row)
		if err == sql.ErrNoRows {
			b = Balance{ProductID: productID, WarehouseID: warehouseID,
				OnHandQty: "0", ReservedQty: "0", AvailableQty: "0"}
			return nil
		}
		if err != nil {
			return fmt.Errorf("查 inventory_balances: %w", err)
		}
		b = *got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// BalanceKey 是 BatchGetBalance 的入参项。
type BalanceKey struct {
	ProductID   string
	WarehouseID string
}

// BatchGetBalance 是组件间批量读余额的唯一方式（防 N+1）。只返回找到的——
// 缺失等价于 0（同 GetBalance），调用方按缺失自行判断为 0。
func (r *Repo) BatchGetBalance(ctx context.Context, keys []BalanceKey) ([]*Balance, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	var out []*Balance
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		productIDs := make([]string, len(keys))
		warehouseIDs := make([]int64, len(keys))
		for i, k := range keys {
			whID, err := parseWarehouseID(k.WarehouseID)
			if err != nil {
				return err
			}
			productIDs[i] = k.ProductID
			warehouseIDs[i] = whID
		}
		// (product_id, warehouse_id) 是复合键：两个数组 + UNNEST 配对逐一匹配，
		// 不用 IN，免得两个 IN 的笛卡尔积多查出无关行。pgx/v5 的 database/sql
		// 驱动直接把 []string / []int64 编码成 PostgreSQL 数组参数。
		rows, err := tx.QueryContext(ctx, `
			SELECT b.product_id, b.warehouse_id, b.on_hand_qty, b.reserved_qty,
				b.on_hand_qty - b.reserved_qty, b.version
			FROM inventory_balances b
			JOIN (SELECT unnest($1::text[]) AS product_id, unnest($2::bigint[]) AS warehouse_id) k
			  ON k.product_id = b.product_id AND k.warehouse_id = b.warehouse_id`,
			productIDs, warehouseIDs)
		if err != nil {
			return fmt.Errorf("查 inventory_balances: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanBalanceRow(rows)
			if err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

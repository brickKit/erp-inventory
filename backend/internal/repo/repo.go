// Package repo 是 erp-inventory 的数据访问层：warehouses / inventory_balances /
// inventory_movements / inventory_reservations / warehouse_access 五张表 +
// Outbox 写入。
//
// ⚠️ 防超卖的判定与加锁是同一条 SQL 语句（条件 UPDATE）——这一层的正确性
// 直接决定库存会不会真的被超卖。
//
// 文件分工：repo.go 放共用的错误、幂等声明与小工具；balance.go 读余额（单条、
// 批量、列表）；movement.go 写入库 / 调整并列流水；reservation.go 是 TCC 三件套；
// stats.go 是仪表盘统计；warehouse.go 读仓库；access.go 管 warehouse_access。
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// ── 哨兵错误。grpc / http 两层通过 service.ToStatus 统一映射 ──

// ErrInvalidArgument 是入参本身不合法。
var ErrInvalidArgument = errors.New("参数不合法")

// ErrNotFound：按 id 查不到。
var ErrNotFound = errors.New("not found")

// ErrInsufficientStock 是 Reserve / Adjust 的条件更新 RowsAffected()==0 时
// 返回的错误，映射成 FailedPrecondition。⚠️ 它也覆盖"(product, warehouse)
// 从没有过余额行"的情形（等价于库存为 0），不需要单独分辨。
var ErrInsufficientStock = errors.New("库存不足")

// ErrForbidden：调用者对某个具体仓库没有 warehouse_access 授权。⚠️ 只用于
// "点名一个具体仓库、但没有权限"的场景（GetBalance / Receive / Adjust）——
// 不是 ErrNotFound：那个仓库真实存在，调用者只是看不见，两种语义不能混用。
var ErrForbidden = errors.New("无权访问该仓库")

// Repo 持有共享池 + 本组件的 role / schema，每个事务都经 besdk.WithTx 切换。
type Repo struct {
	db     *sql.DB
	role   string
	schema string
}

func New(db *sql.DB, role, schema string) *Repo {
	return &Repo{db: db, role: role, schema: schema}
}

// ── 幂等：先声明再干活（claim-first），不是"先查后插" ──
//
// 先原子声明（INSERT ... ON CONFLICT DO NOTHING），声明成功才做真正的写。
// "先查后插"下，两个带同一个 idempotency_key 的并发请求都可能在"查不到"的
// 窗口里各自跑一遍条件更新，Reserve 就变成重复预留。claim 靠
// command_idempotency.idempotency_key 的主键约束把并发请求排成队：后到的
// INSERT 被数据库挂起，直到先到的事务提交或回滚，不会两边都"以为自己是
// 第一次"。
func claimIdempotency(ctx context.Context, tx *sql.Tx, key, command string) (claimed bool, err error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO command_idempotency (idempotency_key, command, result_id) VALUES ($1, $2, '')
		 ON CONFLICT (idempotency_key) DO NOTHING`,
		key, command)
	if err != nil {
		return false, fmt.Errorf("声明 command_idempotency: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func finalizeIdempotency(ctx context.Context, tx *sql.Tx, key, resultID string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE command_idempotency SET result_id = $1 WHERE idempotency_key = $2`, resultID, key)
	if err != nil {
		return fmt.Errorf("落地 command_idempotency 结果: %w", err)
	}
	return nil
}

func lookupIdempotencyResult(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var resultID string
	if err := tx.QueryRowContext(ctx,
		`SELECT result_id FROM command_idempotency WHERE idempotency_key = $1`, key).Scan(&resultID); err != nil {
		return "", fmt.Errorf("查 command_idempotency: %w", err)
	}
	return resultID, nil
}

func parseWarehouseID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: warehouse_id 不合法：%q", ErrInvalidArgument, s)
	}
	return id, nil
}

// containsInt64 判断 id 在不在 allowed 里——"点名的这个仓库我有没有权限"。
func containsInt64(allowed []int64, id int64) bool {
	for _, v := range allowed {
		if v == id {
			return true
		}
	}
	return false
}

type rowScanner interface {
	Scan(dest ...any) error
}

// mapBalanceWriteErr 把 warehouse_id 的外键冲突翻成 ErrInvalidArgument——
// 调用方传了一个不存在的仓库，是入参问题，不是内部错误。
func mapBalanceWriteErr(err error) error {
	if isForeignKeyViolation(err) {
		return fmt.Errorf("%w: 仓库不存在", ErrInvalidArgument)
	}
	return fmt.Errorf("写 inventory_balances: %w", err)
}

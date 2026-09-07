// Package partition 是 Module.Start 的后台循环之一：为 event_outbox/
// event_inbox 自动创建未来的周分区（决策 54、§11.5.1）——跨周时分区
// 不存在会让写入直接崩，migrations 里只建了当时那几周的初始分区
// （见 002_create_outbox_inbox.up.sql），往后必须有人接着建。
//
// inventory_movements 的月分区维护在 monthly.go——它和这里的周分区是
// 两套独立的窗口逻辑，不能合并成一份（设计计划 §7：交易流水按月，
// 不是 outbox/inbox 那种周）。
//
// warehouses/inventory_balances/inventory_reservations/
// product_tracking_snapshots 不在这里——它们不分区（设计计划 §2、§7）。
package partition

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
)

const (
	checkInterval  = 24 * time.Hour
	lookAheadWeeks = 4 // 提前建好当前周 + 未来 4 周，留足缓冲
)

var weeklyPartitionedTables = []string{"event_outbox", "event_inbox"}

// Start 立刻检查一次，之后每 24 小时检查一次。单次检查失败只记日志，
// 不让整个循环退出——下一轮还有机会补上，且不能因为这个后台任务死了
// 拖累整个组件（Start 只在 ctx.Done 时返回，§13.3 铁律七）。
func Start(ctx context.Context, db *sql.DB, role, schema string, logger *slog.Logger) error {
	if err := ensureAllWeekly(ctx, db, role, schema); err != nil {
		logger.Error("周分区维护失败", "error", err)
	}

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := ensureAllWeekly(ctx, db, role, schema); err != nil {
				logger.Error("周分区维护失败", "error", err)
			}
		}
	}
}

func ensureAllWeekly(ctx context.Context, db *sql.DB, role, schema string) error {
	return besdk.WithTx(ctx, db, role, schema, func(tx *sql.Tx) error {
		weekStart := mondayOf(time.Now().UTC())
		for i := 0; i <= lookAheadWeeks; i++ {
			from := weekStart.AddDate(0, 0, 7*i)
			to := from.AddDate(0, 0, 7)
			for _, table := range weeklyPartitionedTables {
				if err := ensurePartition(ctx, tx, table, from, to); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// mondayOf 把任意时间点归到它所在周的周一 00:00 UTC——分区边界必须是
// 固定的锚点，不能是"从现在起 7 天"这种滑动窗口，否则相邻两次检查算出
// 来的分区边界会对不上。
func mondayOf(t time.Time) time.Time {
	weekday := int(t.Weekday())
	if weekday == 0 { // time.Sunday == 0，此处要归到"上一周的周一"而不是当天
		weekday = 7
	}
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return d.AddDate(0, 0, -(weekday - 1))
}

// ensurePartition 用 to_regclass 先确认分区存不存在，不存在才建。
//
// ⚠️ 不能反过来"先建、报 already exists 就忽略"——PostgreSQL 里一条
// 语句真的执行失败会让整个事务 aborted，即使这里选择忽略那个错误，
// 事务在数据库那侧也回不去了（同 mdm-product 002 迁移注释的教训）。
func ensurePartition(ctx context.Context, tx *sql.Tx, table string, from, to time.Time) error {
	name := fmt.Sprintf("%s_%s", table, from.Format("2006_01_02"))

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		return fmt.Errorf("检查分区是否存在 %s: %w", name, err)
	}
	if exists {
		return nil
	}

	// table 只来自本文件顶部的固定清单，from/to 是格式化过的日期字符串，
	// 都不是外部输入，拼 SQL 是安全的。
	stmt := fmt.Sprintf(
		`CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
		name, table, from.Format("2006-01-02"), to.Format("2006-01-02"),
	)
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("建分区 %s: %w", name, err)
	}
	return nil
}

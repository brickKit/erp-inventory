// inventory_movements 的月分区维护——本组件独有，其余组件都没有按月
// 分区的表（设计计划 §7、总纲 §11.2.5）。复用 partition.go 的
// ensurePartition（分区名格式、建分区的 SQL 都一样，只是边界的算法从
// "周一"换成"月初"）。
package partition

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
)

const lookAheadMonths = 3 // 提前建好当前月 + 未来 3 个月

var monthlyPartitionedTables = []string{"inventory_movements"}

// StartMonthly 立刻检查一次，之后每 24 小时检查一次。同 Start 的容错
// 方式：单次失败只记日志，不让循环退出。
func StartMonthly(ctx context.Context, db *sql.DB, role, schema string, logger *slog.Logger) error {
	if err := ensureAllMonthly(ctx, db, role, schema); err != nil {
		logger.Error("月分区维护失败", "error", err)
	}

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := ensureAllMonthly(ctx, db, role, schema); err != nil {
				logger.Error("月分区维护失败", "error", err)
			}
		}
	}
}

func ensureAllMonthly(ctx context.Context, db *sql.DB, role, schema string) error {
	return besdk.WithTx(ctx, db, role, schema, func(tx *sql.Tx) error {
		monthStart := firstOfMonth(time.Now().UTC())
		for i := 0; i <= lookAheadMonths; i++ {
			from := monthStart.AddDate(0, i, 0)
			to := from.AddDate(0, 1, 0)
			for _, table := range monthlyPartitionedTables {
				if err := ensurePartition(ctx, tx, table, from, to); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// firstOfMonth 把任意时间点归到它所在月的 1 号 00:00 UTC——同 mondayOf
// 的判据：分区边界必须是固定锚点。
func firstOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

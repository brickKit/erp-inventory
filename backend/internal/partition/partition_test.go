package partition

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestMondayOf(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"2026-09-06", "2026-08-31"}, // Sunday -> 上一周一
		{"2026-08-31", "2026-08-31"}, // Monday -> 自己
		{"2026-09-02", "2026-08-31"}, // Wednesday -> 本周一
	}
	for _, c := range cases {
		in, err := time.Parse("2006-01-02", c.in)
		if err != nil {
			t.Fatal(err)
		}
		want, err := time.Parse("2006-01-02", c.want)
		if err != nil {
			t.Fatal(err)
		}
		got := mondayOf(in)
		if !got.Equal(want) {
			t.Errorf("mondayOf(%s) = %s，期望 %s", c.in, got.Format("2006-01-02"), c.want)
		}
	}
}

func TestFirstOfMonth(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026-09-07", "2026-09-01"},
		{"2026-09-30", "2026-09-01"},
		{"2026-01-01", "2026-01-01"},
	}
	for _, c := range cases {
		in, _ := time.Parse("2006-01-02", c.in)
		want, _ := time.Parse("2006-01-02", c.want)
		if got := firstOfMonth(in); !got.Equal(want) {
			t.Errorf("firstOfMonth(%s) = %s，期望 %s", c.in, got.Format("2006-01-02"), c.want)
		}
	}
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_PG_DSN")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestEnsureAllMonthly_与迁移建的初始分区命名一致不冲突 是设计计划 §9
// 第 8 条那个真 bug 的回归测试：迁移建的初始分区名必须和这里生成的名字
// 完全一样（"表名_YYYY_MM_01"），否则 to_regclass 查不到已存在的分区，
// 会尝试新建同一段时间范围的分区，撞上 PostgreSQL 分区范围不许重叠的
// 报错。这条测试连跑两次 ensureAllMonthly 验证幂等（第二次不该报错、
// 不该产生重复分区），且要求 inventory_movements 表本身已经由迁移建好
// （TEST_PG_DSN 指向的 schema 需要先跑过 001/002/003 迁移）。
func TestEnsureAllMonthly_幂等且与迁移分区命名一致(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	role := os.Getenv("TEST_PG_ROLE")
	if role == "" {
		role = "erp_inventory_rw"
	}
	schema := os.Getenv("TEST_PG_SCHEMA")
	if schema == "" {
		schema = "erp_inventory"
	}

	if err := ensureAllMonthly(ctx, db, role, schema); err != nil {
		t.Fatalf("第一次 ensureAllMonthly 失败（很可能是分区命名与迁移不一致导致的重叠）：%v", err)
	}
	if err := ensureAllMonthly(ctx, db, role, schema); err != nil {
		t.Fatalf("第二次 ensureAllMonthly 失败，说明不是幂等的：%v", err)
	}

	// 未来第 lookAheadMonths 个月的分区应该已经被建出来——证明维护任务
	// 真的在往前建，不只是识别出已有分区就什么都不做。
	// ⚠️ to_regclass 必须带 schema 前缀——db 这个连接没有把 schema 放进
	// search_path（那是 WithTx 内部事务的事），裸表名会在默认 search_path
	// 下解析成"不存在"，即使分区已经在 schema 里建好了。
	future := firstOfMonth(time.Now().UTC()).AddDate(0, lookAheadMonths, 0)
	name := schema + ".inventory_movements_" + future.Format("2006_01_02")
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("期望未来第 %d 个月的分区 %s 已建好，实际不存在", lookAheadMonths, name)
	}
}

package consumer

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-inventory/backend/internal/repo"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
)

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

func natsURLForTest(t *testing.T) string {
	t.Helper()
	if u := os.Getenv("TEST_NATS_URL"); u != "" {
		return u
	}
	return nats.DefaultURL
}

// testSubject 给消费者测试造一个测试私有的 subject，不直接用生产真实
// subject。
//
// ⚠️ 实测踩坑（docs/dev/field-tested-pitfalls-log.md 类别 E 的 E2）：这两条
// 测试原来直接订阅/发布到真实 subject（如 "mdm.product.created.v1"），
// 而同一台机器上 `brickkit up` 真实跑着的 erp-inventory 容器订阅的是
// **同一个** subject——NATS 核心发布订阅对同一 subject 的多个订阅者是
// 广播，两边都会收到测试发布的消息，谁先把 event_inbox 那一行 INSERT
// 成功谁就真正执行 handler，断言读到的可能是真实容器的产出，不是本地
// 被测代码的产出。
//
// 换一个测试私有的 subject 就能让真实容器完全收不到——它们只订阅生产
// subject 字面量，不会去猜一个带随机后缀的名字。这个换法是安全的：
// besdk.Consume 的 fn 只用 ev.Subject 拼错误信息，不拿它做任何业务判断，
// 换成任意字符串不影响被测逻辑本身。这条规避法只适用于"测试直接构造/
// 发布事件"的消费者测试——验证"真的发到了生产 subject 上"这件事本身的
// 测试必须用真实 subject，不适用这个换法（本文件没有这类测试）。
func testSubject(base string) string {
	return fmt.Sprintf("test.%s.%d", base, time.Now().UnixNano())
}

func publishProductEvent(t *testing.T, nc *nats.Conn, subject, productID, trackingType string, version int64) {
	t.Helper()
	payload := fmt.Sprintf(`{"id":%q,"tracking_type":%q,"version":%d}`, productID, trackingType, version)
	msg := &nats.Msg{Subject: subject, Data: []byte(payload), Header: nats.Header{}}
	msg.Header.Set("X-Aggregate-Id", productID)
	msg.Header.Set("X-Version", strconv.FormatInt(version, 10))
	msg.Header.Set("X-Hop-Count", "0")
	if err := nc.PublishMsg(msg); err != nil {
		t.Fatal(err)
	}
}

func getSnapshot(t *testing.T, db *sql.DB, productID string) (trackingType string, version int64, found bool) {
	t.Helper()
	err := besdk.WithTx(context.Background(), db, "erp_inventory_rw", "erp_inventory", func(tx *sql.Tx) error {
		var err error
		trackingType, version, found, err = repo.GetProductTrackingSnapshotTx(tx, productID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

// TestConsumer_created事件维护摘要副本 是设计计划 §4、§5 的直接测试：
// 消费 mdm.product.created.v1 只取 tracking_type，落进
// product_tracking_snapshots。
func TestConsumer_created事件维护摘要副本(t *testing.T) {
	db := testDB(t)
	nc, err := nats.Connect(natsURLForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	productID := fmt.Sprintf("consumer-test-%d", time.Now().UnixNano())
	subj := testSubject("mdm.product.created.v1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = besdk.Consume(ctx, nc, db, "erp_inventory_rw", "erp_inventory", subj, handle)
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)

	publishProductEvent(t, nc, subj, productID, "BATCH", 1)
	nc.Flush()
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done

	trackingType, version, found := getSnapshot(t, db, productID)
	if !found {
		t.Fatal("期望摘要副本已经写入，实际没查到")
	}
	if trackingType != "BATCH" || version != 1 {
		t.Fatalf("期望 tracking_type=BATCH version=1，实际 tracking_type=%q version=%d", trackingType, version)
	}
}

// TestConsumer_version不大于本地当前值时不覆盖 是跨 subject 乱序的测试：
// besdk.Consume 的 event_inbox 只按"同一个 subject"保证单调，
// created.v1(v1) 和 updated.v1(v2) 是两个不同 subject，如果 v2 先到、
// v1 后到（网络乱序），UpsertProductTrackingSnapshotTx 里的
// "WHERE version < EXCLUDED.version" 必须再挡一次，不能让旧版本覆盖新版本。
func TestConsumer_跨subject乱序时旧版本不覆盖新版本(t *testing.T) {
	db := testDB(t)
	nc, err := nats.Connect(natsURLForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	productID := fmt.Sprintf("consumer-test-order-%d", time.Now().UnixNano())
	subjCreated := testSubject("mdm.product.created.v1")
	subjUpdated := testSubject("mdm.product.updated.v1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	doneCreated := make(chan struct{})
	doneUpdated := make(chan struct{})
	go func() {
		_ = besdk.Consume(ctx, nc, db, "erp_inventory_rw", "erp_inventory", subjCreated, handle)
		close(doneCreated)
	}()
	go func() {
		_ = besdk.Consume(ctx, nc, db, "erp_inventory_rw", "erp_inventory", subjUpdated, handle)
		close(doneUpdated)
	}()
	time.Sleep(150 * time.Millisecond)

	// 先发 version=2（updated，模拟"新的"），再发 version=1（created，
	// 模拟"旧的"网络延迟后到）。
	publishProductEvent(t, nc, subjUpdated, productID, "SERIAL", 2)
	nc.Flush()
	time.Sleep(300 * time.Millisecond)
	publishProductEvent(t, nc, subjCreated, productID, "NONE", 1)
	nc.Flush()
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-doneCreated
	<-doneUpdated

	trackingType, version, found := getSnapshot(t, db, productID)
	if !found {
		t.Fatal("期望摘要副本已经写入")
	}
	if version != 2 || trackingType != "SERIAL" {
		t.Fatalf("旧版本(created, v1)不该覆盖新版本(updated, v2)，期望 tracking_type=SERIAL version=2，"+
			"实际 tracking_type=%q version=%d", trackingType, version)
	}
}

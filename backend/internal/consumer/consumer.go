// Package consumer 消费 mdm.product.created.v1/.updated.v1，维护
// product_tracking_snapshots 摘要副本——本组件不依赖 mdm-product，这是唯一
// 一处跨组件耦合，且走事件不走同步调用。
//
// 幂等与乱序分两层：besdk.Consume 用 event_inbox 保证同一个 (subject,
// aggregate_id) 只接受严格更大的 version、重复投递只处理一次；created 与
// updated 是两个 subject，跨 subject 的乱序由 UpsertProductTrackingSnapshotTx
// 的 WHERE version < EXCLUDED.version 再挡一次。
package consumer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/nats-io/nats.go"

	"github.com/brickKit/erp-inventory/v2/backend/internal/repo"
)

// productPayload 只取 tracking_type——mdm.product.created.v1/.updated.v1
// 的 payload 还有 sku/name/standard_cost 等字段，本组件不关心（product_id
// 对本组件是不透明外键，"产品是什么"归 mdm-product）。
type productPayload struct {
	ID           string `json:"id"`
	TrackingType string `json:"tracking_type"`
	Version      int64  `json:"version"`
}

var productSubjects = []string{"mdm.product.created.v1", "mdm.product.updated.v1"}

// Start 订阅两个 subject，各自跑在自己的 goroutine 里——besdk.Consume 是
// 阻塞到 ctx 取消才返回的循环（同 module.go 里 outbox pump / partition
// 维护的写法，多个后台循环并发跑，不能顺序调用）。
func Start(ctx context.Context, db *sql.DB, role, schema string, nc *nats.Conn, logger *slog.Logger) error {
	errCh := make(chan error, len(productSubjects))
	for _, subject := range productSubjects {
		subject := subject
		go func() {
			errCh <- besdk.Consume(ctx, nc, db, role, schema, subject, handle)
		}()
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err // ⚠️ 返回 error，不许 log.Fatal：进外壳后会带走所有成员
	}
}

func handle(_ context.Context, tx *sql.Tx, ev besdk.Event) error {
	var p productPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return fmt.Errorf("解析 %s payload: %w", ev.Subject, err)
	}
	return repo.UpsertProductTrackingSnapshotTx(tx, p.ID, p.TrackingType, p.Version)
}

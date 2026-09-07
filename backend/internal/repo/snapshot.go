package repo

import (
	"database/sql"
	"fmt"
)

// UpsertProductTrackingSnapshotTx 维护 product_tracking_snapshots 摘要
// 副本——只取 tracking_type（设计计划 §5）。供 backend/internal/consumer
// 在 besdk.Consume 给的事务里调用，因此接 *sql.Tx 而不是自己开
// besdk.WithTx（那会开一个新事务，和 Consume 已经打开的那个冲突）。
//
// ⚠️ WHERE version < $3 是按 version 单调更新（§3.10）：besdk.Consume 的
// event_inbox 只在"同一个 subject"内保证单调，created.v1 和 updated.v1
// 是两个不同的 subject，跨 subject 的乱序（updated 先于 created 到达）
// 必须在这一层的 UPSERT 里再挡一次。
func UpsertProductTrackingSnapshotTx(tx *sql.Tx, productID, trackingType string, version int64) error {
	if trackingType == "" {
		trackingType = "NONE"
	}
	_, err := tx.Exec(`
		INSERT INTO product_tracking_snapshots (product_id, tracking_type, version)
		VALUES ($1, $2, $3)
		ON CONFLICT (product_id) DO UPDATE
		   SET tracking_type = EXCLUDED.tracking_type, version = EXCLUDED.version, updated_at = now()
		 WHERE product_tracking_snapshots.version < EXCLUDED.version`,
		productID, trackingType, version)
	if err != nil {
		return fmt.Errorf("写 product_tracking_snapshots: %w", err)
	}
	return nil
}

// GetProductTrackingSnapshotTx 仅供测试/排障用：读回摘要副本当前值。
// 同 UpsertProductTrackingSnapshotTx，接 *sql.Tx 而不是开新事务。
func GetProductTrackingSnapshotTx(tx *sql.Tx, productID string) (trackingType string, version int64, found bool, err error) {
	err = tx.QueryRow(`SELECT tracking_type, version FROM product_tracking_snapshots WHERE product_id = $1`,
		productID).Scan(&trackingType, &version)
	if err == sql.ErrNoRows {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("查 product_tracking_snapshots: %w", err)
	}
	return trackingType, version, true, nil
}

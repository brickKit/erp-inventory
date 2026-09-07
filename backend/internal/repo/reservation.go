// TCC 三件套 + 状态查询——防超卖与防"薛定谔的超时"都在这个文件里
// （设计计划 §3、§4.5）。
package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	besdk "github.com/brickKit/be-sdk-go"
)

// ReservationStatus 的四个值。⚠️ StatusUnspecified 不是"错误"，是
// GetReservationStatus/CancelReservation/ConfirmIssue 对"查不到这个
// reservation_id"的正常回答——上游超时后必须能区分"根本没到"
// （StatusUnspecified，可以安全重试）与"到了且已撤销"（StatusCancelled，
// 不能重试），见设计计划 §3、§4.5。
const (
	StatusUnspecified = "" // NOT_FOUND
	StatusReserved    = "RESERVED"
	StatusConfirmed   = "CONFIRMED"
	StatusCancelled   = "CANCELLED"
)

// ReserveItem 是 Reserve 的一个 (product, warehouse, qty) 项。
type ReserveItem struct {
	ProductID   string
	WarehouseID string
	Qty         string
}

// ReserveInput 对应 ReserveRequest。
type ReserveInput struct {
	IdempotencyKey string
	OrderID        string
	Items          []ReserveItem
}

// Reserve 预留库存：一次调用里的所有项在同一个事务里全部成功或全部失败
// （TCC 第一步，跨组件写接口）。防超卖的判定与加锁是同一条 UPDATE 语句
// （设计计划 §2.2）——不是"先查再写"。
func (r *Repo) Reserve(ctx context.Context, in ReserveInput) (string, error) {
	if len(in.Items) == 0 {
		return "", fmt.Errorf("%w: items 不能为空", ErrInvalidArgument)
	}
	var reservationID string
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		claimed, err := claimIdempotency(ctx, tx, in.IdempotencyKey, "Reserve")
		if err != nil {
			return err
		}
		if !claimed {
			reservationID, err = lookupIdempotencyResult(ctx, tx, in.IdempotencyKey)
			return err
		}

		var groupID int64
		if err := tx.QueryRowContext(ctx, `SELECT nextval('inventory_reservation_seq')`).Scan(&groupID); err != nil {
			return fmt.Errorf("生成 reservation_id: %w", err)
		}

		for _, item := range in.Items {
			whID, err := parseWarehouseID(item.WarehouseID)
			if err != nil {
				return err
			}

			// ⚠️ 防超卖在这一行：判定（够不够）与加锁（这条 UPDATE 本身）
			// 是同一条语句，中间没有窗口（设计计划 §2.2）。
			res, err := tx.ExecContext(ctx, `
				UPDATE inventory_balances
				   SET reserved_qty = reserved_qty + $1, version = version + 1, updated_at = now()
				 WHERE product_id = $2 AND warehouse_id = $3
				   AND on_hand_qty - reserved_qty >= $1`,
				item.Qty, item.ProductID, whID)
			if err != nil {
				return fmt.Errorf("预留 product_id=%s: %w", item.ProductID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("%w: product_id=%s warehouse_id=%s qty=%s",
					ErrInsufficientStock, item.ProductID, item.WarehouseID, item.Qty)
			}

			if _, err := tx.ExecContext(ctx, `
				INSERT INTO inventory_reservations (reservation_id, order_id, product_id, warehouse_id, qty)
				VALUES ($1, $2, $3, $4, $5)`,
				groupID, in.OrderID, item.ProductID, whID, item.Qty); err != nil {
				if isUniqueViolation(err) {
					return fmt.Errorf("%w: items 里出现了重复的 (product_id, warehouse_id)：%s/%s",
						ErrInvalidArgument, item.ProductID, item.WarehouseID)
				}
				return fmt.Errorf("写 inventory_reservations: %w", err)
			}
		}

		reservationID = strconv.FormatInt(groupID, 10)
		return finalizeIdempotency(ctx, tx, in.IdempotencyKey, reservationID)
	})
	if err != nil {
		return "", err
	}
	return reservationID, nil
}

// reservationRow 是 inventory_reservations 一行，Cancel/ConfirmIssue
// 内部装载用。
type reservationRow struct {
	ProductID   string
	WarehouseID int64
	Qty         string
	OrderID     string
}

// loadReservationGroup 按 reservation_id 返回这一组行 + 组内共同的
// status。forUpdate=true 时加行锁——Cancel/ConfirmIssue 必须锁，防止并发
// 的两边都读到 RESERVED 然后都往下改；GetReservationStatus 是纯读，不该
// 背这个锁（它常在超时重试路径上被高频调用，不该被一个正在进行中的
// Cancel/ConfirmIssue 事务卡住——READ COMMITTED 下不加锁一样只会读到
// 已提交的状态，不存在脏读）。空切片 + status=="" 表示这个 reservation_id
// 不存在（NOT_FOUND）。
func loadReservationGroup(ctx context.Context, tx *sql.Tx, reservationID string, forUpdate bool) (rows []reservationRow, status string, orderID string, err error) {
	groupID, err := strconv.ParseInt(reservationID, 10, 64)
	if err != nil {
		return nil, "", "", fmt.Errorf("%w: reservation_id 不合法：%q", ErrInvalidArgument, reservationID)
	}
	query := `SELECT product_id, warehouse_id, qty, order_id, status FROM inventory_reservations WHERE reservation_id = $1`
	if forUpdate {
		query += " FOR UPDATE"
	}
	rs, err := tx.QueryContext(ctx, query, groupID)
	if err != nil {
		return nil, "", "", fmt.Errorf("查 inventory_reservations: %w", err)
	}
	defer rs.Close()
	for rs.Next() {
		var row reservationRow
		var rawWarehouseID int64
		var rowStatus string
		if err := rs.Scan(&row.ProductID, &rawWarehouseID, &row.Qty, &row.OrderID, &rowStatus); err != nil {
			return nil, "", "", err
		}
		row.WarehouseID = rawWarehouseID
		status = rowStatus // 组内所有行共享同一个 status（设计计划 §9 第 6 条的不变式）
		orderID = row.OrderID
		rows = append(rows, row)
	}
	if err := rs.Err(); err != nil {
		return nil, "", "", err
	}
	return rows, status, orderID, nil
}

// CancelReservationInput 对应 CancelReservationRequest。
type CancelReservationInput struct {
	IdempotencyKey string
	ReservationID  string
}

// CancelReservation 释放预留（TCC 补偿动作）。⚠️ 这是一个"状态内省"式的
// 接口：查不到、已经是 CANCELLED、已经 CONFIRMED 都不是错误，直接把
// 当前状态如实返回——错误只留给真正的系统失败（设计计划 §3 的
// CancelReservationResponse 只带 status，没有单独的"错误详情"字段）。
func (r *Repo) CancelReservation(ctx context.Context, in CancelReservationInput) (string, error) {
	var status string
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		claimed, err := claimIdempotency(ctx, tx, in.IdempotencyKey, "CancelReservation")
		if err != nil {
			return err
		}
		if !claimed {
			status, err = lookupIdempotencyResult(ctx, tx, in.IdempotencyKey)
			return err
		}

		rows, current, _, err := loadReservationGroup(ctx, tx, in.ReservationID, true)
		if err != nil {
			return err
		}
		switch current {
		case StatusUnspecified, StatusCancelled, StatusConfirmed:
			// 不存在 / 已撤销 / 已确认：如实返回当前状态，不做任何改动。
			status = current
		default: // StatusReserved
			for _, item := range rows {
				res, err := tx.ExecContext(ctx, `
					UPDATE inventory_balances
					   SET reserved_qty = reserved_qty - $1, version = version + 1, updated_at = now()
					 WHERE product_id = $2 AND warehouse_id = $3 AND reserved_qty >= $1`,
					item.Qty, item.ProductID, item.WarehouseID)
				if err != nil {
					return fmt.Errorf("释放预留 product_id=%s: %w", item.ProductID, err)
				}
				if n, _ := res.RowsAffected(); n == 0 {
					// 不应该发生：Reserve 已经把这份 reserved_qty 加上过。
					// 出现即说明余额表被别处改坏了，属于内部不一致。
					return fmt.Errorf("释放预留时余额不一致：product_id=%s warehouse_id=%d",
						item.ProductID, item.WarehouseID)
				}
			}
			groupID, _ := strconv.ParseInt(in.ReservationID, 10, 64)
			if _, err := tx.ExecContext(ctx, `
				UPDATE inventory_reservations SET status = $1, version = version + 1, updated_at = now()
				WHERE reservation_id = $2`, StatusCancelled, groupID); err != nil {
				return fmt.Errorf("更新 inventory_reservations: %w", err)
			}
			status = StatusCancelled
		}
		return finalizeIdempotency(ctx, tx, in.IdempotencyKey, status)
	})
	if err != nil {
		return "", err
	}
	return status, nil
}

// ConfirmIssueInput 对应 ConfirmIssueRequest。batch_no/serial_no 对整个
// reservation 下的所有项统一生效（设计计划 §9 第 7 条：多项各自需要不同
// 批次/序列号时，调用方应该拆成多个 Reserve/ConfirmIssue）。
type ConfirmIssueInput struct {
	IdempotencyKey string
	ReservationID  string
	BatchNo        string
	SerialNo       string
}

// ConfirmIssue 预留转实际出库：on_hand 减、reserved 减、写流水（一项一条），
// 发 erp.inventory.adjusted.v1。同 CancelReservation，是状态内省接口。
func (r *Repo) ConfirmIssue(ctx context.Context, in ConfirmIssueInput) (status string, movementIDs []string, err error) {
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		claimed, err := claimIdempotency(ctx, tx, in.IdempotencyKey, "ConfirmIssue")
		if err != nil {
			return err
		}
		if !claimed {
			raw, err := lookupIdempotencyResult(ctx, tx, in.IdempotencyKey)
			if err != nil {
				return err
			}
			status, movementIDs = decodeConfirmResult(raw)
			return nil
		}

		rows, current, orderID, err := loadReservationGroup(ctx, tx, in.ReservationID, true)
		if err != nil {
			return err
		}
		switch current {
		case StatusUnspecified, StatusCancelled, StatusConfirmed:
			// 已确认的情形理论上应该走上面 !claimed 的分支拿到历史
			// movement_ids；如果是带着一个新 idempotency_key 打到一个
			// 已经被别的调用确认过的 reservation，这里只如实报告状态，
			// movement_ids 留空——这个边界情形记进设计计划 §9 第 7 条。
			status = current
		default: // StatusReserved
			for _, item := range rows {
				res, err := tx.ExecContext(ctx, `
					UPDATE inventory_balances
					   SET on_hand_qty = on_hand_qty - $1, reserved_qty = reserved_qty - $1,
					       version = version + 1, updated_at = now()
					 WHERE product_id = $2 AND warehouse_id = $3
					   AND reserved_qty >= $1 AND on_hand_qty >= $1`,
					item.Qty, item.ProductID, item.WarehouseID)
				if err != nil {
					return fmt.Errorf("确认出库 product_id=%s: %w", item.ProductID, err)
				}
				if n, _ := res.RowsAffected(); n == 0 {
					return fmt.Errorf("确认出库时余额不一致：product_id=%s warehouse_id=%d",
						item.ProductID, item.WarehouseID)
				}

				id, err := insertMovement(ctx, tx, movementInput{
					ProductID: item.ProductID, WarehouseID: item.WarehouseID,
					Qty: negate(item.Qty), Reason: "ISSUE",
					OrderID: orderID, BatchNo: in.BatchNo, SerialNo: in.SerialNo,
				})
				if err != nil {
					return err
				}
				movementIDStr := strconv.FormatInt(id, 10)
				movementIDs = append(movementIDs, movementIDStr)

				if err := publishAdjusted(tx, r.schema, movementIDStr, item.ProductID,
					strconv.FormatInt(item.WarehouseID, 10), negate(item.Qty), "ISSUE",
					in.BatchNo, in.SerialNo); err != nil {
					return err
				}
			}
			groupID, _ := strconv.ParseInt(in.ReservationID, 10, 64)
			if _, err := tx.ExecContext(ctx, `
				UPDATE inventory_reservations SET status = $1, version = version + 1, updated_at = now()
				WHERE reservation_id = $2`, StatusConfirmed, groupID); err != nil {
				return fmt.Errorf("更新 inventory_reservations: %w", err)
			}
			status = StatusConfirmed
		}
		return finalizeIdempotency(ctx, tx, in.IdempotencyKey, encodeConfirmResult(status, movementIDs))
	})
	if err != nil {
		return "", nil, err
	}
	return status, movementIDs, nil
}

// negate 给带符号的 decimal-as-string 数量取反：出库在流水里记成负数，
// 但预留行的 qty 存的是正数（"占了 3 件"，不是"占了 -3 件"）。
func negate(numeric string) string {
	if len(numeric) > 0 && numeric[0] == '-' {
		return numeric[1:]
	}
	return "-" + numeric
}

// encodeConfirmResult/decodeConfirmResult 把 (status, movementIDs) 编进
// command_idempotency.result_id 这一个 TEXT 列——格式是
// "status|id1,id2,..."，重试时原样解回来,不用为 ConfirmIssue 单独开一张表。
func encodeConfirmResult(status string, movementIDs []string) string {
	joined := ""
	for i, id := range movementIDs {
		if i > 0 {
			joined += ","
		}
		joined += id
	}
	return status + "|" + joined
}

func decodeConfirmResult(raw string) (status string, movementIDs []string) {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '|' {
			status = raw[:i]
			rest := raw[i+1:]
			if rest != "" {
				start := 0
				for j := 0; j <= len(rest); j++ {
					if j == len(rest) || rest[j] == ',' {
						movementIDs = append(movementIDs, rest[start:j])
						start = j + 1
					}
				}
			}
			return status, movementIDs
		}
	}
	return raw, nil
}

// GetReservationStatus 是防"薛定谔的超时"的唯一手段（设计计划 §4.5）：
// 上游 Reserve 超时后必须先查这个接口，严禁直接调 Cancel。
func (r *Repo) GetReservationStatus(ctx context.Context, reservationID string) (status, orderID string, err error) {
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		_, current, oid, err := loadReservationGroup(ctx, tx, reservationID, false)
		status, orderID = current, oid
		return err
	})
	return status, orderID, err
}

package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
)

// Movement 是 inventory_movements 一行的视图。流水只增不改：写入即终态。
type Movement struct {
	ID          string
	ProductID   string
	WarehouseID string
	Qty         string
	Reason      string
	Note        string
	OrderID     string
	BatchNo     string
	SerialNo    string
	CreatedAt   time.Time
}

func scanMovement(rows rowScanner) (*Movement, error) {
	var m Movement
	var rawID, rawWarehouseID int64
	if err := rows.Scan(&rawID, &m.ProductID, &rawWarehouseID, &m.Qty, &m.Reason,
		&m.Note, &m.OrderID, &m.BatchNo, &m.SerialNo, &m.CreatedAt); err != nil {
		return nil, err
	}
	m.ID = strconv.FormatInt(rawID, 10)
	m.WarehouseID = strconv.FormatInt(rawWarehouseID, 10)
	return &m, nil
}

// ReceiveInput 对应 ReceiveRequest。
type ReceiveInput struct {
	IdempotencyKey string
	ProductID      string
	WarehouseID    string
	Qty            string
	BatchNo        string
	SerialNo       string
	// AllowedWarehouseIDs 是调用者的 warehouse_access 授权列表——service 层
	// 从 besdk.ScopeOf(ctx) 取 sub 查出来再传进来，repo 层不碰 ScopeOf
	// （那是验过签的 HTTP 请求才有的东西）。
	AllowedWarehouseIDs []int64
}

// Receive 入库：on_hand 加、写流水、发 erp.inventory.adjusted.v1。
func (r *Repo) Receive(ctx context.Context, in ReceiveInput) (string, error) {
	return r.applyStockChange(ctx, stockChange{
		command: "Receive", idempotencyKey: in.IdempotencyKey,
		productID: in.ProductID, warehouseID: in.WarehouseID, allowed: in.AllowedWarehouseIDs,
		qty: in.Qty, reason: "RECEIVE", batchNo: in.BatchNo, serialNo: in.SerialNo,
	})
}

// AdjustInput 对应 AdjustRequest。QtyDelta 带符号：正为盘盈，负为盘亏。
type AdjustInput struct {
	IdempotencyKey string
	ProductID      string
	WarehouseID    string
	QtyDelta       string
	Reason         string // 人类可读的调整原因（如"盘点差异"），落进 movements.note
	// AllowedWarehouseIDs 见 ReceiveInput 同名字段。
	AllowedWarehouseIDs []int64
}

// Adjust 盘盈盘亏：条件更新同时守住"不能调到负库存"与"不能调到低于已预留的
// 量"两条不变式——判定写在 WHERE 里，和 Reserve 同一个风格，不是写完再检查。
func (r *Repo) Adjust(ctx context.Context, in AdjustInput) (string, error) {
	reason := "ADJUST_GAIN"
	if isNegative(in.QtyDelta) {
		reason = "ADJUST_LOSS"
	}
	return r.applyStockChange(ctx, stockChange{
		command: "Adjust", idempotencyKey: in.IdempotencyKey,
		productID: in.ProductID, warehouseID: in.WarehouseID, allowed: in.AllowedWarehouseIDs,
		qty: in.QtyDelta, guarded: true, reason: reason, note: in.Reason,
	})
}

// stockChange 是 Receive / Adjust 共用的一次在手量变动。
type stockChange struct {
	command        string // 写进 command_idempotency.command
	idempotencyKey string
	productID      string
	warehouseID    string
	allowed        []int64
	qty            string // 带符号的十进制字符串
	// guarded：条件更新要守"不低于 0、不低于已预留量"（Adjust）。Receive 的
	// qty 由 service 层保证为正，不需要这道判定。
	guarded  bool
	reason   string // inventory_movements.reason
	note     string
	batchNo  string
	serialNo string
}

// applyStockChange 在一个事务里做完：声明幂等键 → 校验仓库授权 → 确保余额行
// 存在 → 条件更新在手量 → 写流水 → 进 Outbox → 落地幂等结果。重复的幂等键
// 直接返回第一次的 movement_id。
func (r *Repo) applyStockChange(ctx context.Context, c stockChange) (string, error) {
	var movementID string
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		claimed, err := claimIdempotency(ctx, tx, c.idempotencyKey, c.command)
		if err != nil {
			return err
		}
		if !claimed {
			movementID, err = lookupIdempotencyResult(ctx, tx, c.idempotencyKey)
			return err
		}

		whID, err := parseWarehouseID(c.warehouseID)
		if err != nil {
			return err
		}
		if !containsInt64(c.allowed, whID) {
			return ErrForbidden
		}

		// 先确保余额行存在（这个 (product, warehouse) 第一次进货），再用统一的
		// 条件更新写入——"第一次"和"不是第一次"走同一段代码。
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO inventory_balances (product_id, warehouse_id, on_hand_qty)
			VALUES ($1, $2, 0) ON CONFLICT (product_id, warehouse_id) DO NOTHING`,
			c.productID, whID); err != nil {
			return mapBalanceWriteErr(err)
		}
		if err := updateOnHand(ctx, tx, c, whID); err != nil {
			return err
		}

		id, err := insertMovement(ctx, tx, movementInput{
			ProductID: c.productID, WarehouseID: whID, Qty: c.qty,
			Reason: c.reason, Note: c.note, BatchNo: c.batchNo, SerialNo: c.serialNo,
		})
		if err != nil {
			return err
		}
		movementID = strconv.FormatInt(id, 10)

		if err := publishAdjusted(tx, r.schema, movementID, c.productID, c.warehouseID,
			c.qty, c.reason, c.batchNo, c.serialNo); err != nil {
			return err
		}
		return finalizeIdempotency(ctx, tx, c.idempotencyKey, movementID)
	})
	if err != nil {
		return "", err
	}
	return movementID, nil
}

func updateOnHand(ctx context.Context, tx *sql.Tx, c stockChange, whID int64) error {
	if !c.guarded {
		if _, err := tx.ExecContext(ctx, `
			UPDATE inventory_balances
			   SET on_hand_qty = on_hand_qty + $1, version = version + 1, updated_at = now()
			 WHERE product_id = $2 AND warehouse_id = $3`,
			c.qty, c.productID, whID); err != nil {
			return fmt.Errorf("更新 inventory_balances: %w", err)
		}
		return nil
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE inventory_balances
		   SET on_hand_qty = on_hand_qty + $1, version = version + 1, updated_at = now()
		 WHERE product_id = $2 AND warehouse_id = $3
		   AND on_hand_qty + $1 >= reserved_qty AND on_hand_qty + $1 >= 0`,
		c.qty, c.productID, whID)
	if err != nil {
		return fmt.Errorf("更新 inventory_balances: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: 调整会让库存低于 0 或低于已预留量（product_id=%s warehouse_id=%s qty_delta=%s）",
			ErrInsufficientStock, c.productID, c.warehouseID, c.qty)
	}
	return nil
}

// isNegative 只看符号，不把十进制字符串转成浮点数。service 层已校验过它是
// 合法的非零数字。
func isNegative(numeric string) bool {
	return strings.HasPrefix(strings.TrimSpace(numeric), "-")
}

type movementInput struct {
	ProductID   string
	WarehouseID int64
	Qty         string
	Reason      string
	Note        string
	OrderID     string
	BatchNo     string
	SerialNo    string
}

func insertMovement(ctx context.Context, tx *sql.Tx, in movementInput) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO inventory_movements (product_id, warehouse_id, qty, reason, note, order_id, batch_no, serial_no)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		in.ProductID, in.WarehouseID, in.Qty, in.Reason, in.Note, in.OrderID, in.BatchNo, in.SerialNo,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写 inventory_movements: %w", err)
	}
	return id, nil
}

// publishAdjusted 发 erp.inventory.adjusted.v1：aggregate_id 是 movement_id——
// 每条流水都是一个只出现一次的聚合根，version 恒为 1（流水只增不改，没有
// "同一条流水的第二个版本"）。⚠️ payload 只带数量不带金额——erp-finance
// 消费它，自己按成本方法算凭证金额。
func publishAdjusted(tx *sql.Tx, schema, movementID, productID, warehouseID, qtyDelta, reason, batchNo, serialNo string) error {
	payload, err := json.Marshal(map[string]any{
		"product_id": productID, "warehouse_id": warehouseID, "qty_delta": qtyDelta,
		"movement_id": movementID, "batch_no": batchNo, "serial_no": serialNo, "reason": reason,
	})
	if err != nil {
		return err
	}
	return besdk.PublishOutbox(tx, schema, besdk.Event{
		Subject: "erp.inventory.adjusted.v1", AggregateID: movementID, Version: 1, Payload: payload,
	})
}

// ListInput 对应 ListMovementsRequest。刻意没有 offset：只能用游标往后翻。
type ListInput struct {
	Cursor        string
	PageSize      int
	ProductID     string
	WarehouseID   string
	CreatedAfter  time.Time
	CreatedBefore time.Time
	// AllowedWarehouseIDs 是调用者的 warehouse_access 授权列表——**必须**下推
	// 进 SQL 的 WHERE，不能查出来再在 Go 里过滤：那会破坏分页（一页可能被
	// 滤成空页，next_cursor 却还在）。
	AllowedWarehouseIDs []int64
}

type ListResult struct {
	Movements  []*Movement
	NextCursor string
}

// listMovementsSQL 是 ListMovements 唯一的一条静态查询：可选过滤写成
// "参数为空就不限"，不在 Go 里拼 SQL。
//
// ⚠️ warehouse_id = ANY($3) 永远在——调用者没有任何授权时 $3 是空数组，
// 天然匹配不到任何行，这就是"没分配 = 谁都看不见"的失败关闭结果。
const listMovementsSQL = `
	SELECT id, product_id, warehouse_id, qty, reason, note, order_id, batch_no, serial_no, created_at
	  FROM inventory_movements
	 WHERE created_at >= $1 AND created_at <= $2
	   AND warehouse_id = ANY($3::bigint[])
	   AND ($4::text = '' OR product_id = $4::text)
	   AND ($5::bigint IS NULL OR warehouse_id = $5::bigint)
	   AND ($6::timestamptz IS NULL OR (created_at, id) < ($6::timestamptz, $7::bigint))
	 ORDER BY created_at DESC, id DESC
	 LIMIT $8`

func (r *Repo) ListMovements(ctx context.Context, in ListInput) (*ListResult, error) {
	q := besdk.ListWindow(besdk.Query{
		From: in.CreatedAfter, To: in.CreatedBefore, Cursor: in.Cursor, Limit: in.PageSize,
	})

	var cursorAt, cursorID any // 没有游标时两个都是 NULL
	if q.Cursor != "" {
		ck, err := decodeCursor(q.Cursor)
		if err != nil {
			return nil, fmt.Errorf("%w: 非法 cursor：%v", ErrInvalidArgument, err)
		}
		cursorAt, cursorID = ck.CreatedAt, ck.ID
	}
	var warehouseFilter any // 不按仓库过滤时是 NULL
	if in.WarehouseID != "" {
		whID, err := parseWarehouseID(in.WarehouseID)
		if err != nil {
			return nil, err
		}
		warehouseFilter = whID
	}
	allowed := in.AllowedWarehouseIDs
	if allowed == nil {
		allowed = []int64{}
	}

	var out ListResult
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		// 多取一条，用来判断是否还有下一页。
		rows, err := tx.QueryContext(ctx, listMovementsSQL,
			q.From, q.To, allowed, in.ProductID, warehouseFilter, cursorAt, cursorID, q.Limit+1)
		if err != nil {
			return fmt.Errorf("查 inventory_movements: %w", err)
		}
		defer rows.Close()

		var movements []*Movement
		for rows.Next() {
			m, err := scanMovement(rows)
			if err != nil {
				return err
			}
			movements = append(movements, m)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		if len(movements) > q.Limit {
			last := movements[q.Limit-1]
			lastRawID, err := strconv.ParseInt(last.ID, 10, 64)
			if err != nil {
				return err
			}
			out.NextCursor = encodeCursor(cursorKey{CreatedAt: last.CreatedAt, ID: lastRawID})
			movements = movements[:q.Limit]
		}
		out.Movements = movements
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

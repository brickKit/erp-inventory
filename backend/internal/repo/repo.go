// Package repo 是 erp-inventory 的数据访问层：warehouses/inventory_balances/
// inventory_movements/inventory_reservations 四张表 + Outbox 写入。
//
// ⚠️ 防超卖的判定与加锁是同一条 SQL 语句（条件 UPDATE，设计计划 §2.2）——
// 这一层的正确性直接决定库存会不会真的被超卖，不是"接对了没有"这么轻的
// 责任（同 mdm-product 的判据，但风险等级不同）。
package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
)

// ── 哨兵错误。grpc/http 两层通过 service.ToStatus 统一映射（同 mdm-product）──

// ErrInvalidArgument 是入参本身不合法。
var ErrInvalidArgument = errors.New("参数不合法")

// ErrNotFound：按 id 查不到。
var ErrNotFound = errors.New("not found")

// ErrInsufficientStock 是 Reserve/Adjust 的条件更新 RowsAffected()==0
// 时返回的错误——判据见设计计划 §2.2："RowsAffected() == 0 就是库存
// 不足，返回 FailedPrecondition"。⚠️ 这个错误也覆盖"(product,warehouse)
// 从没有过余额行"的情形（等价于库存为 0），不需要单独分辨。
var ErrInsufficientStock = errors.New("库存不足")

// Repo 持有共享池 + 本组件的 role/schema，写操作一律经 besdk.WithTx 切换。
type Repo struct {
	db     *sql.DB
	role   string
	schema string
}

func New(db *sql.DB, role, schema string) *Repo {
	return &Repo{db: db, role: role, schema: schema}
}

// ── 幂等：claim-then-work，不是 mdm-product 那种"先查后插"的弱形式 ──
//
// ⚠️ 这里比 mdm-product 的 lookupIdempotency 多做一步：先原子声明
// （INSERT ... ON CONFLICT DO NOTHING），声明成功才做真正的写操作。
// 原因：Reserve 的副作用（占库存）比 mdm-product 的 Create 重得多——
// "先查后插"下，两个带着同一个 idempotency_key 的并发请求都可能在
// "查不到"的窗口里各自跑一遍条件更新，变成重复预留。claim 用
// command_idempotency.idempotency_key 的主键约束天然序列化并发请求：
// 后到的那个 INSERT 会被数据库挂起直到先到的事务提交/回滚，因此永远不会
// 出现两边都"以为自己是第一次"的窗口。
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

// ── 余额 ──

// Balance 是 inventory_balances 一行的视图。AvailableQty 服务端算好直接
// 给，不让调用方自己减（设计计划 §3 的 Balance 消息注释）。
type Balance struct {
	ProductID    string
	WarehouseID  string
	OnHandQty    string
	ReservedQty  string
	AvailableQty string
	Version      int64
}

// GetBalance：查不到余额行等价于库存为 0（从没收过货），不是错误——
// 一个从没进过货的 (product, warehouse) 组合问"还有多少"，答案就是 0。
func (r *Repo) GetBalance(ctx context.Context, productID, warehouseID string) (*Balance, error) {
	whID, err := parseWarehouseID(warehouseID)
	if err != nil {
		return nil, err
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

type rowScanner interface {
	Scan(dest ...any) error
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

// BalanceKey 是 BatchGetBalance 的入参项。
type BalanceKey struct {
	ProductID   string
	WarehouseID string
}

// BatchGetBalance 是防 N+1 的唯一合法调用方式（§3.8）。只返回找到的——
// 缺失等价于 0（同 GetBalance 的判据），调用方按缺失自行判断为 0。
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
		// (product_id, warehouse_id) 是复合键，用两个数组 + UNNEST 展开配对
		// 逐一匹配，比拼一长串 OR 更安全（不用担心 IN 笛卡尔积多查出无关行）。
		// ⚠️ pgx/v5 的 database/sql 驱动能直接把 []string/[]int64 编码成
		// PostgreSQL 数组参数，不需要额外包一层 pq.Array 之类的类型。
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

// ── 流水 ──

// Movement 是 inventory_movements 一行的视图。
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
}

// Receive 入库：on_hand 加、写流水、发 erp.inventory.adjusted.v1。
func (r *Repo) Receive(ctx context.Context, in ReceiveInput) (string, error) {
	var movementID string
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		claimed, err := claimIdempotency(ctx, tx, in.IdempotencyKey, "Receive")
		if err != nil {
			return err
		}
		if !claimed {
			movementID, err = lookupIdempotencyResult(ctx, tx, in.IdempotencyKey)
			return err
		}

		whID, err := parseWarehouseID(in.WarehouseID)
		if err != nil {
			return err
		}

		// 先确保余额行存在（首次收货这个 (product, warehouse) 组合），
		// 再用统一的条件更新写入——同一段代码路径处理"第一次"和"不是
		// 第一次"，不用分支。
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO inventory_balances (product_id, warehouse_id, on_hand_qty)
			VALUES ($1, $2, 0) ON CONFLICT (product_id, warehouse_id) DO NOTHING`,
			in.ProductID, whID); err != nil {
			return mapBalanceWriteErr(err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE inventory_balances
			   SET on_hand_qty = on_hand_qty + $1, version = version + 1, updated_at = now()
			 WHERE product_id = $2 AND warehouse_id = $3`,
			in.Qty, in.ProductID, whID); err != nil {
			return fmt.Errorf("更新 inventory_balances: %w", err)
		}

		id, err := insertMovement(ctx, tx, movementInput{
			ProductID: in.ProductID, WarehouseID: whID, Qty: in.Qty,
			Reason: "RECEIVE", BatchNo: in.BatchNo, SerialNo: in.SerialNo,
		})
		if err != nil {
			return err
		}
		movementID = strconv.FormatInt(id, 10)

		if err := publishAdjusted(tx, r.schema, movementID, in.ProductID, in.WarehouseID,
			in.Qty, "RECEIVE", in.BatchNo, in.SerialNo); err != nil {
			return err
		}
		return finalizeIdempotency(ctx, tx, in.IdempotencyKey, movementID)
	})
	if err != nil {
		return "", err
	}
	return movementID, nil
}

// AdjustInput 对应 AdjustRequest。QtyDelta 带符号：正为盘盈，负为盘亏。
type AdjustInput struct {
	IdempotencyKey string
	ProductID      string
	WarehouseID    string
	QtyDelta       string
	Reason         string // 人类可读的调整原因（如"盘点差异"），落进 movements.note
}

// Adjust 盘盈盘亏：条件更新同时守住"不能调到负库存"与"不能调到低于已预留
// 的量"两条不变式——判定写在 WHERE 里，和 Reserve 同一个风格（设计计划
// §2.2），不是插入之后再检查。
func (r *Repo) Adjust(ctx context.Context, in AdjustInput) (string, error) {
	var movementID string
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		claimed, err := claimIdempotency(ctx, tx, in.IdempotencyKey, "Adjust")
		if err != nil {
			return err
		}
		if !claimed {
			movementID, err = lookupIdempotencyResult(ctx, tx, in.IdempotencyKey)
			return err
		}

		whID, err := parseWarehouseID(in.WarehouseID)
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO inventory_balances (product_id, warehouse_id, on_hand_qty)
			VALUES ($1, $2, 0) ON CONFLICT (product_id, warehouse_id) DO NOTHING`,
			in.ProductID, whID); err != nil {
			return mapBalanceWriteErr(err)
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE inventory_balances
			   SET on_hand_qty = on_hand_qty + $1, version = version + 1, updated_at = now()
			 WHERE product_id = $2 AND warehouse_id = $3
			   AND on_hand_qty + $1 >= reserved_qty AND on_hand_qty + $1 >= 0`,
			in.QtyDelta, in.ProductID, whID)
		if err != nil {
			return fmt.Errorf("更新 inventory_balances: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: 调整会让库存低于 0 或低于已预留量（product_id=%s warehouse_id=%s qty_delta=%s）",
				ErrInsufficientStock, in.ProductID, in.WarehouseID, in.QtyDelta)
		}

		reason := "ADJUST_GAIN"
		if isNegative(in.QtyDelta) {
			reason = "ADJUST_LOSS"
		}
		id, err := insertMovement(ctx, tx, movementInput{
			ProductID: in.ProductID, WarehouseID: whID, Qty: in.QtyDelta,
			Reason: reason, Note: in.Reason,
		})
		if err != nil {
			return err
		}
		movementID = strconv.FormatInt(id, 10)

		if err := publishAdjusted(tx, r.schema, movementID, in.ProductID, in.WarehouseID,
			in.QtyDelta, reason, "", ""); err != nil {
			return err
		}
		return finalizeIdempotency(ctx, tx, in.IdempotencyKey, movementID)
	})
	if err != nil {
		return "", err
	}
	return movementID, nil
}

func isNegative(numeric string) bool {
	f, err := strconv.ParseFloat(numeric, 64)
	return err == nil && f < 0
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

// publishAdjusted 发 erp.inventory.adjusted.v1（设计计划 §4）：aggregate_id
// 是 movement_id——每条流水都是一个新的、只出现一次的聚合根，version 恒
// 为 1（流水只增不改，不存在"同一个 movement 的第二个版本"）。⚠️ payload
// 只带数量不带金额——erp-finance 消费它自己按成本方法算出凭证金额。
func publishAdjusted(tx *sql.Tx, schema, movementID, productID, warehouseID, qtyDelta, reason, batchNo, serialNo string) error {
	payload, err := json.Marshal(map[string]any{
		"product_id": productID, "warehouse_id": warehouseID, "qty_delta": qtyDelta,
		"movement_id": movementID, "batch_no": batchNo, "serial_no": serialNo, "reason": reason,
	})
	if err != nil {
		return err
	}
	if err := besdk.PublishOutbox(tx, schema, besdk.Event{
		Subject: "erp.inventory.adjusted.v1", AggregateID: movementID, Version: 1, Payload: payload,
	}); err != nil {
		return err
	}
	return nil
}

// ListInput 对应 ListMovementsRequest。刻意没有 offset 字段（决策 53）。
type ListInput struct {
	Cursor        string
	PageSize      int
	ProductID     string
	WarehouseID   string
	CreatedAfter  time.Time
	CreatedBefore time.Time
}

type ListResult struct {
	Movements  []*Movement
	NextCursor string
}

func buildListQuery(in ListInput) besdk.Query {
	return besdk.ListWindow(besdk.Query{
		From: in.CreatedAfter, To: in.CreatedBefore, Cursor: in.Cursor, Limit: in.PageSize,
	})
}

func (r *Repo) ListMovements(ctx context.Context, in ListInput) (*ListResult, error) {
	q := buildListQuery(in)

	var ck *cursorKey
	if q.Cursor != "" {
		decoded, err := decodeCursor(q.Cursor)
		if err != nil {
			return nil, fmt.Errorf("非法 cursor：%w", err)
		}
		ck = &decoded
	}

	var out ListResult
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		query := `SELECT id, product_id, warehouse_id, qty, reason, note, order_id, batch_no, serial_no, created_at
			FROM inventory_movements
			WHERE created_at >= $1 AND created_at <= $2`
		args := []any{q.From, q.To}
		if in.ProductID != "" {
			args = append(args, in.ProductID)
			query += fmt.Sprintf(" AND product_id = $%d", len(args))
		}
		if in.WarehouseID != "" {
			whID, err := parseWarehouseID(in.WarehouseID)
			if err != nil {
				return err
			}
			args = append(args, whID)
			query += fmt.Sprintf(" AND warehouse_id = $%d", len(args))
		}
		if ck != nil {
			args = append(args, ck.CreatedAt, ck.ID)
			query += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
		}
		args = append(args, q.Limit+1) // 多取一条，用来判断是否还有下一页
		query += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))

		rows, err := tx.QueryContext(ctx, query, args...)
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

// mapBalanceWriteErr 把 warehouse_id 的外键冲突翻成 ErrInvalidArgument——
// 调用方传了一个不存在的仓库，是入参问题，不是内部错误。
func mapBalanceWriteErr(err error) error {
	if isForeignKeyViolation(err) {
		return fmt.Errorf("%w: 仓库不存在", ErrInvalidArgument)
	}
	return fmt.Errorf("写 inventory_balances: %w", err)
}

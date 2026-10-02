// Package service 是 erp-inventory 的业务规则层：入参校验 + 给 http/grpc
// 一个不依赖 repo 内部细节的稳定入口。真正的乐观锁判断、防超卖条件更新、
// 事件发布都在 repo 层随 SQL 一起做（同一个事务里），同 mdm-product 的
// 判据——这一层依然薄。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-inventory/v2/backend/internal/repo"
)

// allowedWarehouseIDs 是所有按 warehouse 维过滤的端点（Receive / Adjust /
// GetBalance / ListMovements / ListWarehouses / ListBalances / StockSummary）
// 共用的一步：从 besdk.ScopeOf(ctx) 取 sub、查 warehouse_access 表拿到这个人
// 当前能访问的仓库列表。⚠️ 只有经过 RequirePermission 验签的 REST 请求才有
// Claims：Reserve / CancelReservation / ConfirmIssue / BatchGetBalance 是组件间
// gRPC 协议，不调它；同名读写方法经 gRPC 直连时 ctx 里没有 Claims，ScopeOf
// panic、被 SDK 的恢复拦截器接成 Internal——失败关闭，不会多返回数据。gRPC 上
// 按调用者透传数据范围是另一件事，见 docs/design.md 的未决问题。
func (s *Service) allowedWarehouseIDs(ctx context.Context) ([]int64, error) {
	sub := besdk.ScopeOf(ctx).Owner
	return s.repo.WarehouseIDsFor(ctx, sub)
}

// ErrInvalidArgument 是入参本身不合法。
var ErrInvalidArgument = errors.New("参数不合法")

type Service struct {
	repo              *repo.Repo
	logger            *slog.Logger
	lowStockThreshold string
}

func New(r *repo.Repo, logger *slog.Logger, opts ...Option) *Service {
	s := &Service{repo: r, logger: logger, lowStockThreshold: DefaultLowStockThreshold}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// decimalQty 是数量入参唯一接受的写法：十进制字符串，可带负号，最多 12 位整数、
// 6 位小数——与列类型 NUMERIC(18,6) 对齐，超出的位数不会被悄悄舍入或溢出成 500。
//
// ⚠️ 不能用 strconv.ParseFloat 校验：它放行 "NaN"、"Inf"、"1e3"、"0x1p4"。NaN
// 进了 NUMERIC 排在所有数之上，on_hand_qty 一旦是 NaN 就永远是 NaN，CHECK 与
// 防超卖条件对这一行恒真，从此可以无限超卖。
var decimalQty = regexp.MustCompile(`^-?[0-9]{1,12}(\.[0-9]{1,6})?$`)

// validateQty 只看字符串，不转浮点：先核对写法，再按字符判断符号与是否为 0
// （"0.000"、"-0" 也是 0）。allowNegative 只给 Adjust 的 qty_delta（盘亏）。
func validateQty(field, s string, allowNegative bool) error {
	if s == "" {
		return fmt.Errorf("%w: %s 不能为空", ErrInvalidArgument, field)
	}
	if !decimalQty.MatchString(s) {
		return fmt.Errorf("%w: %s 不是合法十进制数（最多 12 位整数、6 位小数；不收 NaN / Inf / 科学计数法）：%q",
			ErrInvalidArgument, field, s)
	}
	zero := strings.Trim(s, "-0.") == ""
	if !allowNegative && (zero || strings.HasPrefix(s, "-")) {
		return fmt.Errorf("%w: %s 必须为正数：%q", ErrInvalidArgument, field, s)
	}
	if allowNegative && zero {
		return fmt.Errorf("%w: %s 不能为 0", ErrInvalidArgument, field)
	}
	return nil
}

// ── TCC 三件套 + 状态查询 ──

func (s *Service) Reserve(ctx context.Context, in repo.ReserveInput) (string, error) {
	if in.IdempotencyKey == "" {
		return "", fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	if len(in.Items) == 0 {
		return "", fmt.Errorf("%w: items 不能为空", ErrInvalidArgument)
	}
	for _, item := range in.Items {
		if item.ProductID == "" {
			return "", fmt.Errorf("%w: product_id 不能为空", ErrInvalidArgument)
		}
		if item.WarehouseID == "" {
			return "", fmt.Errorf("%w: warehouse_id 不能为空", ErrInvalidArgument)
		}
		if err := validateQty("qty", item.Qty, false); err != nil {
			return "", err
		}
	}
	reservationID, err := s.repo.Reserve(ctx, in)
	if err != nil {
		s.logger.Error("预留库存失败", "order_id", in.OrderID, "error", err)
		return "", err
	}
	return reservationID, nil
}

func (s *Service) CancelReservation(ctx context.Context, in repo.CancelReservationInput) (string, error) {
	if in.IdempotencyKey == "" {
		return "", fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	if in.ReservationID == "" {
		return "", fmt.Errorf("%w: reservation_id 不能为空", ErrInvalidArgument)
	}
	status, err := s.repo.CancelReservation(ctx, in)
	if err != nil {
		s.logger.Error("释放预留失败", "reservation_id", in.ReservationID, "error", err)
		return "", err
	}
	return status, nil
}

func (s *Service) ConfirmIssue(ctx context.Context, in repo.ConfirmIssueInput) (status string, movementIDs []string, err error) {
	if in.IdempotencyKey == "" {
		return "", nil, fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	if in.ReservationID == "" {
		return "", nil, fmt.Errorf("%w: reservation_id 不能为空", ErrInvalidArgument)
	}
	status, movementIDs, err = s.repo.ConfirmIssue(ctx, in)
	if err != nil {
		s.logger.Error("确认出库失败", "reservation_id", in.ReservationID, "error", err)
		return "", nil, err
	}
	return status, movementIDs, nil
}

// GetReservationStatus 是上游超时之后判断"请求到底有没有生效"的唯一手段——
// 查不到（NOT_FOUND）不是错误，是这个接口存在的意义本身，不在这里拦截。
//
// ⚠️ reservationID / idempotencyKey 二选一：Reserve 本身超时时调用方拿不到
// reservation_id，只能带着当初发的 idempotency_key 来查。两个都不给才是入参
// 错误。返回 resolvedReservationID：走 idempotencyKey 分支时调用方必须拿到
// 真正的 reservation_id，存下来供后续 Cancel / ConfirmIssue 用。
func (s *Service) GetReservationStatus(ctx context.Context, reservationID, idempotencyKey string) (status, orderID, resolvedReservationID string, err error) {
	if reservationID == "" && idempotencyKey == "" {
		return "", "", "", fmt.Errorf("%w: reservation_id 与 idempotency_key 不能同时为空", ErrInvalidArgument)
	}
	return s.repo.GetReservationStatus(ctx, reservationID, idempotencyKey)
}

// ── 命令：入库 / 调整 ──

func (s *Service) Receive(ctx context.Context, in repo.ReceiveInput) (string, error) {
	if in.IdempotencyKey == "" {
		return "", fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	if in.ProductID == "" {
		return "", fmt.Errorf("%w: product_id 不能为空", ErrInvalidArgument)
	}
	if in.WarehouseID == "" {
		return "", fmt.Errorf("%w: warehouse_id 不能为空", ErrInvalidArgument)
	}
	if err := validateQty("qty", in.Qty, false); err != nil {
		return "", err
	}
	allowed, err := s.allowedWarehouseIDs(ctx)
	if err != nil {
		return "", err
	}
	in.AllowedWarehouseIDs = allowed
	movementID, err := s.repo.Receive(ctx, in)
	if err != nil {
		s.logger.Error("入库失败", "product_id", in.ProductID, "warehouse_id", in.WarehouseID, "error", err)
		return "", err
	}
	return movementID, nil
}

func (s *Service) Adjust(ctx context.Context, in repo.AdjustInput) (string, error) {
	if in.IdempotencyKey == "" {
		return "", fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	if in.ProductID == "" {
		return "", fmt.Errorf("%w: product_id 不能为空", ErrInvalidArgument)
	}
	if in.WarehouseID == "" {
		return "", fmt.Errorf("%w: warehouse_id 不能为空", ErrInvalidArgument)
	}
	if err := validateQty("qty_delta", in.QtyDelta, true); err != nil {
		return "", err
	}
	allowed, err := s.allowedWarehouseIDs(ctx)
	if err != nil {
		return "", err
	}
	in.AllowedWarehouseIDs = allowed
	movementID, err := s.repo.Adjust(ctx, in)
	if err != nil {
		s.logger.Error("库存调整失败", "product_id", in.ProductID, "warehouse_id", in.WarehouseID, "error", err)
		return "", err
	}
	return movementID, nil
}

// ── 读 ──

func (s *Service) GetBalance(ctx context.Context, productID, warehouseID string) (*repo.Balance, error) {
	if productID == "" || warehouseID == "" {
		return nil, fmt.Errorf("%w: product_id/warehouse_id 不能为空", ErrInvalidArgument)
	}
	allowed, err := s.allowedWarehouseIDs(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.GetBalance(ctx, productID, warehouseID, allowed)
}

// BatchGetBalance 是组件间 gRPC 协议（erp-sales 等的批量读），不经过
// besdk.RequirePermission，ctx 里没有 Claims——不调 allowedWarehouseIDs，
// 见该方法注释。
func (s *Service) BatchGetBalance(ctx context.Context, keys []repo.BalanceKey) ([]*repo.Balance, error) {
	return s.repo.BatchGetBalance(ctx, keys)
}

func (s *Service) ListMovements(ctx context.Context, in repo.ListInput) (*repo.ListResult, error) {
	allowed, err := s.allowedWarehouseIDs(ctx)
	if err != nil {
		return nil, err
	}
	in.AllowedWarehouseIDs = allowed
	return s.repo.ListMovements(ctx, in)
}

// ── warehouse_access 管理 ──

func (s *Service) ListWarehouseAccess(ctx context.Context, sub string) ([]int64, error) {
	if sub == "" {
		return nil, fmt.Errorf("%w: sub 不能为空", ErrInvalidArgument)
	}
	return s.repo.WarehouseIDsFor(ctx, sub)
}

func (s *Service) GrantWarehouseAccess(ctx context.Context, sub, warehouseID string) error {
	if sub == "" || warehouseID == "" {
		return fmt.Errorf("%w: sub/warehouse_id 不能为空", ErrInvalidArgument)
	}
	return s.repo.GrantWarehouseAccess(ctx, sub, warehouseID)
}

func (s *Service) RevokeWarehouseAccess(ctx context.Context, sub, warehouseID string) error {
	if sub == "" || warehouseID == "" {
		return fmt.Errorf("%w: sub/warehouse_id 不能为空", ErrInvalidArgument)
	}
	return s.repo.RevokeWarehouseAccess(ctx, sub, warehouseID)
}

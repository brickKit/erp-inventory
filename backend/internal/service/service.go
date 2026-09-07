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
	"strconv"

	"github.com/brickKit/erp-inventory/backend/internal/repo"
)

// ErrInvalidArgument 是入参本身不合法（同 mdm-product 的判据）。
var ErrInvalidArgument = errors.New("参数不合法")

type Service struct {
	repo   *repo.Repo
	logger *slog.Logger
}

func New(r *repo.Repo, logger *slog.Logger) *Service {
	return &Service{repo: r, logger: logger}
}

func validateQty(field, s string, allowNegative bool) error {
	if s == "" {
		return fmt.Errorf("%w: %s 不能为空", ErrInvalidArgument, field)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("%w: %s 不是合法数字：%q", ErrInvalidArgument, field, s)
	}
	if !allowNegative && f <= 0 {
		return fmt.Errorf("%w: %s 必须为正数：%q", ErrInvalidArgument, field, s)
	}
	if allowNegative && f == 0 {
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

// GetReservationStatus 是防"薛定谔的超时"的唯一手段——查不到（NOT_FOUND）
// 不是错误，是这个接口存在的意义本身（设计计划 §4.5），不在这里拦截。
//
// ⚠️ reservationID/idempotencyKey 二选一（设计计划 §9）：Reserve 本身
// 超时时调用方拿不到 reservation_id，只能带着当初发的 idempotency_key
// 来查。两个都不给才是入参错误。
func (s *Service) GetReservationStatus(ctx context.Context, reservationID, idempotencyKey string) (status, orderID string, err error) {
	if reservationID == "" && idempotencyKey == "" {
		return "", "", fmt.Errorf("%w: reservation_id 与 idempotency_key 不能同时为空", ErrInvalidArgument)
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
	return s.repo.GetBalance(ctx, productID, warehouseID)
}

func (s *Service) BatchGetBalance(ctx context.Context, keys []repo.BalanceKey) ([]*repo.Balance, error) {
	return s.repo.BatchGetBalance(ctx, keys)
}

func (s *Service) ListMovements(ctx context.Context, in repo.ListInput) (*repo.ListResult, error) {
	return s.repo.ListMovements(ctx, in)
}

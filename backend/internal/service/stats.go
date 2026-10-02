package service

import (
	"context"
	"fmt"
	"regexp"

	"github.com/brickKit/erp-inventory/v2/backend/internal/repo"
)

// DefaultLowStockThreshold 与 component.yaml 里 LOW_STOCK_THRESHOLD 的默认值相同。
const DefaultLowStockThreshold = "10"

// lowStockLimit 是仪表盘低库存清单最多列出的行数；总数另由 LowStockCount 给出。
const lowStockLimit = 50

// Option 调整 Service 的可配置项。
type Option func(*Service)

// WithLowStockThreshold 设定低库存阈值（非负十进制字符串，先用
// ValidateLowStockThreshold 校验）。
func WithLowStockThreshold(threshold string) Option {
	return func(s *Service) { s.lowStockThreshold = threshold }
}

var nonNegativeDecimal = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// ValidateLowStockThreshold 校验阈值是合法的非负十进制数（"10"、"4.5"）。不接受
// 科学计数法、正负号与首尾空白——它会原样作为 numeric 参数进 SQL。
func ValidateLowStockThreshold(threshold string) error {
	if !nonNegativeDecimal.MatchString(threshold) {
		return fmt.Errorf("%w: 低库存阈值 %q 不是非负十进制数", ErrInvalidArgument, threshold)
	}
	return nil
}

// ListWarehouses 返回调用者有授权的仓库。
func (s *Service) ListWarehouses(ctx context.Context) ([]*repo.Warehouse, error) {
	allowed, err := s.allowedWarehouseIDs(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.ListWarehouses(ctx, allowed)
}

// ListBalances 列出调用者有授权的仓库里的余额。
func (s *Service) ListBalances(ctx context.Context, in repo.BalanceListInput) (*repo.BalanceListResult, error) {
	allowed, err := s.allowedWarehouseIDs(ctx)
	if err != nil {
		return nil, err
	}
	in.AllowedWarehouseIDs = allowed
	return s.repo.ListBalances(ctx, in)
}

// StockSummary 统计调用者有授权的仓库，低库存按本服务配置的阈值判定。
func (s *Service) StockSummary(ctx context.Context) (*repo.StockSummary, error) {
	allowed, err := s.allowedWarehouseIDs(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.StockSummary(ctx, allowed, s.lowStockThreshold, lowStockLimit)
}

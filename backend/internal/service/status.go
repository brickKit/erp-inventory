package service

import (
	"errors"

	"github.com/brickKit/erp-inventory/v2/backend/internal/repo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ToStatus 把 repo 层的哨兵错误翻成 gRPC status——HTTP 与 gRPC 两条对外
// 接口共用同一套业务错误类型（HTTP 由 SDK 引擎的中间件再翻成状态码）。
//
// ⚠️ ErrInsufficientStock → FailedPrecondition（HTTP 400）：条件更新
// RowsAffected() == 0 就是库存不足，调用方据此决定补货或拒单，不是服务端故障。
func ToStatus(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, repo.ErrInsufficientStock):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, repo.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrInvalidArgument), errors.Is(err, repo.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, repo.ErrForbidden):
		return status.Error(codes.PermissionDenied, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

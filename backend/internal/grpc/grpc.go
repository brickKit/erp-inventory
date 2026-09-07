// Package grpc 实现 erp.inventory.v1.InventoryService——内部 gRPC 面
// （§2.1）。HTTP 与 gRPC 共用同一个 service.Service，业务逻辑只写一遍。
package grpc

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	inventoryv1 "github.com/brickKit/erp-inventory/gen/erp/inventory/v1"

	"github.com/brickKit/erp-inventory/backend/internal/repo"
	"github.com/brickKit/erp-inventory/backend/internal/service"
)

type server struct {
	inventoryv1.UnimplementedInventoryServiceServer
	svc *service.Service
}

// New 构造 gRPC 服务端实现。module.go 用它注册到 grpc.Server。
func New(svc *service.Service) inventoryv1.InventoryServiceServer {
	return &server{svc: svc}
}

func toProtoStatus(s string) inventoryv1.ReservationStatus {
	switch s {
	case repo.StatusReserved:
		return inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED
	case repo.StatusConfirmed:
		return inventoryv1.ReservationStatus_RESERVATION_STATUS_CONFIRMED
	case repo.StatusCancelled:
		return inventoryv1.ReservationStatus_RESERVATION_STATUS_CANCELLED
	default:
		// ⚠️ UNSPECIFIED 就是 NOT_FOUND 的信号，不是"忘了填"（设计计划
		// §3、§4.5）。
		return inventoryv1.ReservationStatus_RESERVATION_STATUS_UNSPECIFIED
	}
}

func (s *server) Reserve(ctx context.Context, req *inventoryv1.ReserveRequest) (*inventoryv1.ReserveResponse, error) {
	items := make([]repo.ReserveItem, 0, len(req.Items))
	for _, it := range req.Items {
		items = append(items, repo.ReserveItem{
			ProductID: it.ProductId, WarehouseID: it.WarehouseId, Qty: it.Qty,
		})
	}
	reservationID, err := s.svc.Reserve(ctx, repo.ReserveInput{
		IdempotencyKey: req.IdempotencyKey, OrderID: req.OrderId, Items: items,
	})
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return &inventoryv1.ReserveResponse{ReservationId: reservationID}, nil
}

func (s *server) CancelReservation(ctx context.Context, req *inventoryv1.CancelReservationRequest) (*inventoryv1.CancelReservationResponse, error) {
	status, err := s.svc.CancelReservation(ctx, repo.CancelReservationInput{
		IdempotencyKey: req.IdempotencyKey, ReservationID: req.ReservationId,
	})
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return &inventoryv1.CancelReservationResponse{Status: toProtoStatus(status)}, nil
}

func (s *server) ConfirmIssue(ctx context.Context, req *inventoryv1.ConfirmIssueRequest) (*inventoryv1.ConfirmIssueResponse, error) {
	status, movementIDs, err := s.svc.ConfirmIssue(ctx, repo.ConfirmIssueInput{
		IdempotencyKey: req.IdempotencyKey, ReservationID: req.ReservationId,
		BatchNo: req.BatchNo, SerialNo: req.SerialNo,
	})
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return &inventoryv1.ConfirmIssueResponse{Status: toProtoStatus(status), MovementIds: movementIDs}, nil
}

func (s *server) GetReservationStatus(ctx context.Context, req *inventoryv1.GetReservationStatusRequest) (*inventoryv1.GetReservationStatusResponse, error) {
	status, orderID, err := s.svc.GetReservationStatus(ctx, req.ReservationId)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return &inventoryv1.GetReservationStatusResponse{Status: toProtoStatus(status), OrderId: orderID}, nil
}

func (s *server) Receive(ctx context.Context, req *inventoryv1.ReceiveRequest) (*inventoryv1.ReceiveResponse, error) {
	movementID, err := s.svc.Receive(ctx, repo.ReceiveInput{
		IdempotencyKey: req.IdempotencyKey, ProductID: req.ProductId, WarehouseID: req.WarehouseId,
		Qty: req.Qty, BatchNo: req.BatchNo, SerialNo: req.SerialNo,
	})
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return &inventoryv1.ReceiveResponse{MovementId: movementID}, nil
}

func (s *server) Adjust(ctx context.Context, req *inventoryv1.AdjustRequest) (*inventoryv1.AdjustResponse, error) {
	movementID, err := s.svc.Adjust(ctx, repo.AdjustInput{
		IdempotencyKey: req.IdempotencyKey, ProductID: req.ProductId, WarehouseID: req.WarehouseId,
		QtyDelta: req.QtyDelta, Reason: req.Reason,
	})
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return &inventoryv1.AdjustResponse{MovementId: movementID}, nil
}

func toProtoBalance(b *repo.Balance) *inventoryv1.Balance {
	return &inventoryv1.Balance{
		ProductId: b.ProductID, WarehouseId: b.WarehouseID,
		OnHandQty: b.OnHandQty, ReservedQty: b.ReservedQty, AvailableQty: b.AvailableQty,
		Version: b.Version,
	}
}

func (s *server) GetBalance(ctx context.Context, req *inventoryv1.GetBalanceRequest) (*inventoryv1.Balance, error) {
	b, err := s.svc.GetBalance(ctx, req.ProductId, req.WarehouseId)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return toProtoBalance(b), nil
}

// BatchGetBalance 是防 N+1 的唯一合法调用方式（§3.8）。
func (s *server) BatchGetBalance(ctx context.Context, req *inventoryv1.BatchGetBalanceRequest) (*inventoryv1.BatchGetBalanceResponse, error) {
	keys := make([]repo.BalanceKey, 0, len(req.Keys))
	for _, k := range req.Keys {
		keys = append(keys, repo.BalanceKey{ProductID: k.ProductId, WarehouseID: k.WarehouseId})
	}
	balances, err := s.svc.BatchGetBalance(ctx, keys)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	out := make([]*inventoryv1.Balance, 0, len(balances))
	for _, b := range balances {
		out = append(out, toProtoBalance(b))
	}
	return &inventoryv1.BatchGetBalanceResponse{Balances: out}, nil
}

func toProtoMovement(m *repo.Movement) *inventoryv1.Movement {
	reason := inventoryv1.MovementReason_MOVEMENT_REASON_UNSPECIFIED
	switch m.Reason {
	case "RECEIVE":
		reason = inventoryv1.MovementReason_MOVEMENT_REASON_RECEIVE
	case "ISSUE":
		reason = inventoryv1.MovementReason_MOVEMENT_REASON_ISSUE
	case "ADJUST_GAIN":
		reason = inventoryv1.MovementReason_MOVEMENT_REASON_ADJUST_GAIN
	case "ADJUST_LOSS":
		reason = inventoryv1.MovementReason_MOVEMENT_REASON_ADJUST_LOSS
	}
	return &inventoryv1.Movement{
		Id: m.ID, ProductId: m.ProductID, WarehouseId: m.WarehouseID, Qty: m.Qty,
		Reason: reason, OrderId: m.OrderID, BatchNo: m.BatchNo, SerialNo: m.SerialNo,
		CreatedAt: timestamppb.New(m.CreatedAt),
	}
}

func (s *server) ListMovements(ctx context.Context, req *inventoryv1.ListMovementsRequest) (*inventoryv1.ListMovementsResponse, error) {
	in := repo.ListInput{
		Cursor: req.Cursor, PageSize: int(req.PageSize),
		ProductID: req.ProductId, WarehouseID: req.WarehouseId,
	}
	if req.CreatedAfter != nil {
		in.CreatedAfter = req.CreatedAfter.AsTime()
	}
	if req.CreatedBefore != nil {
		in.CreatedBefore = req.CreatedBefore.AsTime()
	}
	out, err := s.svc.ListMovements(ctx, in)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	movements := make([]*inventoryv1.Movement, 0, len(out.Movements))
	for _, m := range out.Movements {
		movements = append(movements, toProtoMovement(m))
	}
	return &inventoryv1.ListMovementsResponse{Movements: movements, NextCursor: out.NextCursor}, nil
}

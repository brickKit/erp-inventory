// Package http 是 erp-inventory 的 REST 面（对外路径前缀 /erp/inventory，
// 与 assembly.yaml 的 edge_routes 一致）。⚠️ 只暴露 Receive/Adjust/
// GetBalance/ListMovements——TCC 四件套（Reserve/CancelReservation/
// ConfirmIssue/GetReservationStatus）永远不进 REST，它们是组件间协议，
// 不是人类操作（contracts/inventory.openapi.yaml、设计计划 §3）。
package http

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-inventory/backend/internal/repo"
	"github.com/brickKit/erp-inventory/backend/internal/service"
)

// RegisterRoutes 挂载业务路由。
//
// 阶段三 Task 6：权限键从阶段二的 besdk.Public 换成 assembly.yaml 里
// 声明的真实键。⚠️ `erp.inventory.reserve`/`.issue` 两个已声明的权限键
// 在这里用不上——`Reserve`/`ConfirmIssue` 永远不进 REST（组件间 TCC
// 协议，不是人类操作，见包文档），它们只是权限目录里的条目，供菜单/
// 审计展示，不对应任何 besdk.GET/POST 调用点，这是设计使然不是漏填。
// `warehouse` 维数据范围（谁能看哪个仓库）另见 scope.go——不经
// besdk.ScopeOf（那是纯读 JWT 的 org/owner 两维），本组件自己查
// warehouse_access 表。
func RegisterRoutes(eng *gin.Engine, svc *service.Service) {
	g := eng.Group("/erp/inventory")
	besdk.POST(g, "/movements/receive", "erp.inventory.receive", receiveHandler(svc))
	besdk.POST(g, "/movements/adjust", "erp.inventory.adjust", adjustHandler(svc))
	besdk.GET(g, "/balances", "erp.inventory.view", getBalanceHandler(svc))
	besdk.GET(g, "/movements", "erp.inventory.view", listMovementsHandler(svc))
	besdk.GET(g, "/warehouse-access/:sub", "erp.inventory.manage_access", listWarehouseAccessHandler(svc))
	besdk.POST(g, "/warehouse-access/:sub", "erp.inventory.manage_access", grantWarehouseAccessHandler(svc))
	besdk.DELETE(g, "/warehouse-access/:sub/:warehouse_id", "erp.inventory.manage_access", revokeWarehouseAccessHandler(svc))
}

type receiveRequest struct {
	IdempotencyKey string `json:"idempotency_key" binding:"required"`
	ProductID      string `json:"product_id" binding:"required"`
	WarehouseID    string `json:"warehouse_id" binding:"required"`
	Qty            string `json:"qty" binding:"required"`
	BatchNo        string `json:"batch_no"`
	SerialNo       string `json:"serial_no"`
}

func receiveHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req receiveRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		movementID, err := svc.Receive(c.Request.Context(), repo.ReceiveInput{
			IdempotencyKey: req.IdempotencyKey, ProductID: req.ProductID, WarehouseID: req.WarehouseID,
			Qty: req.Qty, BatchNo: req.BatchNo, SerialNo: req.SerialNo,
		})
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"movement_id": movementID})
	}
}

type adjustRequest struct {
	IdempotencyKey string `json:"idempotency_key" binding:"required"`
	ProductID      string `json:"product_id" binding:"required"`
	WarehouseID    string `json:"warehouse_id" binding:"required"`
	QtyDelta       string `json:"qty_delta" binding:"required"`
	Reason         string `json:"reason"`
}

func adjustHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req adjustRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		movementID, err := svc.Adjust(c.Request.Context(), repo.AdjustInput{
			IdempotencyKey: req.IdempotencyKey, ProductID: req.ProductID, WarehouseID: req.WarehouseID,
			QtyDelta: req.QtyDelta, Reason: req.Reason,
		})
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		c.JSON(http.StatusOK, gin.H{"movement_id": movementID})
	}
}

func toBalanceDTO(b *repo.Balance) gin.H {
	return gin.H{
		"product_id": b.ProductID, "warehouse_id": b.WarehouseID,
		"on_hand_qty": b.OnHandQty, "reserved_qty": b.ReservedQty, "available_qty": b.AvailableQty,
		"version": b.Version,
	}
}

func getBalanceHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		b, err := svc.GetBalance(c.Request.Context(), c.Query("product_id"), c.Query("warehouse_id"))
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		c.JSON(http.StatusOK, toBalanceDTO(b))
	}
}

const rfc3339 = "2006-01-02T15:04:05.999999999Z07:00"

func toMovementDTO(m *repo.Movement) gin.H {
	return gin.H{
		"id": m.ID, "product_id": m.ProductID, "warehouse_id": m.WarehouseID, "qty": m.Qty,
		"reason": m.Reason, "order_id": m.OrderID, "batch_no": m.BatchNo, "serial_no": m.SerialNo,
		"created_at": m.CreatedAt.Format(rfc3339),
	}
}

func listMovementsHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		pageSize, _ := strconv.Atoi(c.Query("page_size"))
		out, err := svc.ListMovements(c.Request.Context(), repo.ListInput{
			Cursor: c.Query("cursor"), PageSize: pageSize,
			ProductID: c.Query("product_id"), WarehouseID: c.Query("warehouse_id"),
		})
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		dtos := make([]gin.H, 0, len(out.Movements))
		for _, m := range out.Movements {
			dtos = append(dtos, toMovementDTO(m))
		}
		c.JSON(http.StatusOK, gin.H{"movements": dtos, "next_cursor": out.NextCursor})
	}
}

// ── warehouse_access 管理（阶段三 Task 6，erp.inventory.manage_access）──

func listWarehouseAccessHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ids, err := svc.ListWarehouseAccess(c.Request.Context(), c.Param("sub"))
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		// REST 契约里 id 一律是字符串（同 warehouse_id 在 Balance/Movement
		// DTO 里的既有约定）。
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = strconv.FormatInt(id, 10)
		}
		c.JSON(http.StatusOK, gin.H{"warehouse_ids": out})
	}
}

type grantWarehouseAccessRequest struct {
	WarehouseID string `json:"warehouse_id" binding:"required"`
}

func grantWarehouseAccessHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req grantWarehouseAccessRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := svc.GrantWarehouseAccess(c.Request.Context(), c.Param("sub"), req.WarehouseID); err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		c.Status(http.StatusOK)
	}
}

func revokeWarehouseAccessHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		err := svc.RevokeWarehouseAccess(c.Request.Context(), c.Param("sub"), c.Param("warehouse_id"))
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		c.Status(http.StatusOK)
	}
}

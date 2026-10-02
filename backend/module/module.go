// Package module 是 erp-inventory 唯一的装配入口（全局约束 §K、设计书
// §12.5.1、§13.3 铁律七）。单跑与合并走同一个 New 函数；模块只交回零件
// （handler、gRPC 注册函数、迁移、后台循环），谁去 Listen、谁开池、
// 谁 init OTel、谁装信号处理器，全归调用方。
package module

import (
	"context"
	"fmt"

	besdk "github.com/brickKit/be-sdk-go"
	inventoryv1 "github.com/brickKit/erp-inventory/gen/erp/inventory/v1"
	"google.golang.org/grpc"

	"github.com/brickKit/erp-inventory/v2/backend/internal/consumer"
	grpcapi "github.com/brickKit/erp-inventory/v2/backend/internal/grpc"
	httpapi "github.com/brickKit/erp-inventory/v2/backend/internal/http"
	"github.com/brickKit/erp-inventory/v2/backend/internal/partition"
	"github.com/brickKit/erp-inventory/v2/backend/internal/repo"
	"github.com/brickKit/erp-inventory/v2/backend/internal/service"
)

// New 构造 erp-inventory 模块。签名一个字都不许改（§12.5.1）——62 个
// 组件都是这一个签名，外壳启动器与 be-ops 产出 4 都按它生成。
func New(ctx context.Context, rt *besdk.Runtime) (*besdk.Module, error) {
	// ⚠️ 配置只从 rt.Config 来，模块里零 os.Getenv（§12.5.3、决策 110）。
	schema := rt.Config.StringOr("PG_SCHEMA", "erp_inventory")
	role := schema + "_rw"

	// ⚠️ 池从 rt.DB 来，不许自己 sql.Open（§13.3 铁律二）。
	threshold, err := lowStockThreshold(rt.Config)
	if err != nil {
		return nil, err
	}
	rt.Logger.Info("低库存阈值", "LOW_STOCK_THRESHOLD", threshold)

	r := repo.New(rt.DB, role, schema)
	svc := service.New(r, rt.Logger, service.WithLowStockThreshold(threshold))

	// HTTP：engine 必须用 besdk.NewGinEngine，它已挂好 OTel / request-id /
	// error→status / PII 脱敏日志 / RED 指标 / /healthz / /metrics。
	eng := besdk.NewGinEngine(rt)
	httpapi.RegisterRoutes(eng, svc)

	return &besdk.Module{
		HTTPHandler: eng,

		// ⚠️ gRPC 一个不省，而且由调用方在 extraPorts["grpc"] 上 Listen
		// （§1.5 原则一）。
		RegisterGRPC: func(gs *grpc.Server) {
			inventoryv1.RegisterInventoryServiceServer(gs, grpcapi.New(svc))
		},

		// 后台循环：Outbox 推送 + 周分区维护（event_outbox/event_inbox）+
		// 月分区维护（inventory_movements）+ 消费 mdm.product 事件维护
		// 摘要副本。四个循环必须并发跑，不能顺序调用——它们各自是阻塞到
		// ctx 取消才返回的循环。
		Start: func(ctx context.Context) error {
			errCh := make(chan error, 4)
			go func() { errCh <- besdk.StartOutboxPump(ctx, rt.DB, schema, rt.NATS, rt.Logger) }()
			go func() { errCh <- partition.Start(ctx, rt.DB, role, schema, rt.Logger) }()
			go func() { errCh <- partition.StartMonthly(ctx, rt.DB, role, schema, rt.Logger) }()
			go func() { errCh <- consumer.Start(ctx, rt.DB, role, schema, rt.NATS, rt.Logger) }()

			select {
			case <-ctx.Done():
				return nil
			case err := <-errCh:
				return err // ⚠️ 返回 error，不许 log.Fatal：一个模块退进程 = 整组组件一起没了
			}
		},
		Stop: func(ctx context.Context) error { return nil }, // 后台循环靠 ctx 退出
	}, nil
}

// lowStockThreshold 读 LOW_STOCK_THRESHOLD：没配或空串用默认值；配了非法值返回
// 错误让模块启动失败，而不是悄悄退回默认值跑。
func lowStockThreshold(cfg besdk.Config) (string, error) {
	v := cfg.StringOr("LOW_STOCK_THRESHOLD", "")
	if v == "" {
		return service.DefaultLowStockThreshold, nil
	}
	if err := service.ValidateLowStockThreshold(v); err != nil {
		return "", fmt.Errorf("配置项 LOW_STOCK_THRESHOLD：%w", err)
	}
	return v, nil
}

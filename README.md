# erp-inventory · 库存管理

库存余额、库存流水、库位/仓库、库存预留、防超卖判定——设计书 §2.6 称本组件为**物理命令枢纽**。

## 它能做什么
- 库存入库（`Receive`）、盘盈盘亏调整（`Adjust`）
- 库存预留/释放/确认出库（`Reserve`/`CancelReservation`/`ConfirmIssue`，供 `erp-sales` 等组件的 TCC 链调用）
- 余额与流水查询（`GetBalance`/`BatchGetBalance`/`ListMovements`）

## 需要哪些基础资源
| 资源 | 形态 | 为什么需要 | 怎么起 |
|---|---|---|---|
| PostgreSQL 16 | **A**（brickKit 基础资源，`kind: database`） | 数据持久化，独占 schema `erp_inventory` | 装配仓库根目录 `make up` |
| NATS 2.10 | **A**（`kind: mq`） | 发布 `erp.inventory.*` 事件（Outbox 推送） | 同上 |

⚠️ 形态 A / B / C 的区别见设计书 §2.7.0。本组件**不需要** Traefik 与 Casdoor
就能单独跑起来——它不对 IAM 建依赖边，JWT 走本地验签（决策 87）。

## 怎么起来

```bash
# 装配仓库根目录
make up
cd components/erp/inventory
go build -o build/migrate ./backend/cmd/migrate
PG_SCHEMA=erp_inventory DATABASE_HOST=localhost DATABASE_PORT=5432 \
  DATABASE_USER=postgres DATABASE_PASSWORD=<.env 里的 POSTGRES_PASSWORD> DATABASE_NAME=brickkit_db \
  ./build/migrate up
go run ./backend/cmd/server     # 单独跑：besdk.RunStandalone 读 component.yaml 的端口
```

或者用平台：`brickkit up`（装配仓库根目录，`components/erp/inventory` 登记为 submodule 且在 `brickkit.yaml` 里之后）。也可以直接 `make seed`/`make db-reset`（见根 `docs/dev/种子数据一览.md`）——自成一体的演示数据，不依赖任何其他组件先起来。

## 怎么用

```bash
# 入库（REST，人类操作）
curl -X POST -H 'Authorization: Bearer <应用 token>' -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"recv-demo-1","product_id":"1","warehouse_id":"1","qty":"100"}' \
  http://localhost:8086/erp/inventory/movements/receive

# 查余额
curl -H 'Authorization: Bearer <应用 token>' \
  'http://localhost:8086/erp/inventory/balances?product_id=1&warehouse_id=1'

# 预留库存（gRPC，组件间 TCC 协议，永不暴露到 REST，人类不直接调）
grpcurl -plaintext -d '{
  "idempotency_key": "reserve-demo-1",
  "order_id": "order-1",
  "items": [{"product_id": "1", "warehouse_id": "1", "qty": "5"}]
}' localhost:9096 erp.inventory.v1.InventoryService/Reserve
```

## 配置项

| 配置键 | 默认值 | 说明 |
|---|---|---|
| `pgSchema` | `erp_inventory` | 本组件的 PG schema |
| `otelBaseUrl` | `""` | 空 = Blackhole Exporter，零成本 |
| `iamJwksUrl` | `""` | JWT 本地验签的公钥来源，指向 `infra-iam-casdoor` |
| `authzBundleUrl` | `""` | 权限判定的 bundle 轮询地址，指向 `infra-authz` |

## 参考实现
| 项目 | 看的模块 | 借鉴了什么 | 许可证（已复核） | 用法 |
|---|---|---|---|---|
| ERPNext | `erpnext/stock/stock_ledger.py` | "流水 + 余额缓存"的两层结构；以及它 repost 机制的代价——这是我们决定不支持补录的直接依据 | GPL-3 | 借鉴逻辑 |
| Odoo 17.0 | `addons/stock/models/stock_quant.py` | `available = quantity - reserved` 这个模型；以及它并发处理的反面教训（没有唯一约束，靠事后合并清理重复行） | LGPL-3 | 借鉴逻辑 |
| Odoo 17.0 | `addons/stock/models/stock_move.py` | 状态机的状态划分，确认只有 `done` 与 `cancel` 是终态 | LGPL-3 | 借鉴逻辑 |

**要避免它的什么**：ERPNext 的流水号称不可变，实际 `repost` 机制会 `UPDATE` 历史行且级联无界；
Odoo 的 `stock.quant` 键上没有唯一约束，并发下产生重复行。我们的流水真的只增不改，
`(product_id, warehouse_id)` 上有唯一约束 + 条件更新防超卖。

**⚠️ TCC 式的 `Reserve`/`CancelReservation`/`GetReservationStatus` 三件套，两个参考系统里都没有对应物**
——它们是单体单事务，没有这个问题。这三个接口的正确性只能靠我们自己的并发测试保证。

## 边界与禁令
- 本组件**不查产品数据**——`product_id` 是不透明外键，校验产品存在是调用方（`erp-sales`）的事；
  产品的 `tracking_type` 走事件摘要副本，不建同步依赖边（设计计划 §5）
- 本组件**不算金额**——存货的会计价值归 `erp-finance`，本组件只发数量变动事件
- `inventory_balances` 永不分区永不归档，必须永远小而快（§11.2.5）；`inventory_movements` 只增不改，
  补录历史用冲销流水，不改写过去

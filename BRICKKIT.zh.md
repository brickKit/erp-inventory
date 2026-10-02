# erp/inventory

## 组件定位

回答"这个产品此刻在这个仓库里还有多少、我能不能占一些"的唯一来源：库存余额、库存流水、带防超卖的预留，以及它们所在的仓库。它是实物库存变动的唯一写入方；销售等组件请它预留、释放、出库，自己从不改库存。

**拥有**

- 仓库（由迁移播种：`WH-EAST`、`WH-SOUTH`；没有管理接口），以及谁能访问哪个仓库（`warehouse_access`）。
- 每个（产品，仓库）的余额：在手、已预留、可用 = 在手 − 已预留，一律十进制字符串。预留永远不会超过在手量：判定与加锁是同一条条件 `UPDATE`。
- 库存流水：入库、出库、盘盈、盘亏。只增不改；纠错是再记一笔，从不改旧行。
- 预留：`Reserve` → `ConfirmIssue` 或 `CancelReservation`，超时之后用 `GetReservationStatus` 查请求到底生效没有。
- 每个产品追踪方式（无 / 批次 / 序列号）的摘要副本，靠 `mdm/product` 的事件维护。
- 事件 `erp.inventory.adjusted.v1`；subject `erp.inventory.transferred.v1` 为调拨预留，目前不发布。

**不拥有**

- 产品是什么（SKU、名称、单位、追踪方式）：归 `mdm/product`。这里的 `product_id` 是不透明 ID，核对它存在是调用方的事。
- 库存为什么变动（订单、生产工单、采购到货）：归 `erp/sales` 等 ERP 组件。本组件收到的是"占 3 件"，不是那张单。
- 存货的价值与会计处理：归 `erp/finance`，它消费 `erp.inventory.adjusted.v1`。流水只带数量，从不带金额。

## 部署前准备

- **PostgreSQL**：schema `erp_inventory`；如果你的 schema 约定会建 `erp_inventory_archive` 也可以建（本组件从不写它）。登录角色 `erp_inventory_rw`，在 `erp_inventory` 上有 `USAGE` 与 `CREATE`，以及它的密码。迁移以这个角色运行并建表，所以表归它所有；运行中的组件会自己给 `inventory_movements` 建月分区、给 `event_outbox` / `event_inbox` 建周分区，这需要表的所有权。迁移同时建好两个播种仓库。brickKit 不建这些；在 BrickEnterprise 装配项目里，`make dev-env` 把密码写进 `.env`，`make db-init` 建 schema、角色与授权。
- **NATS** 可经 `NATS_URL` 访问：组件经 Outbox 表与后台循环发布事件，并消费 `mdm.product.created.v1` / `mdm.product.updated.v1`。NATS 不可达时组件照常启动，事件留在 Outbox 里等待。
- **权限**（`infra/authz`）与**身份**（`infra/iam-casdoor`，或任何提供 JWKS 的 IAM）可经 `AUTHZ_BUNDLE_URL`、`IAM_JWKS_URL` 访问，REST 路由才会返回错误以外的结果。它们是配置不是依赖：没有它们组件照样启动。
- **仓库授权**：用户只能看到、只能变动授给自己的仓库里的库存。用 `POST /erp/inventory/warehouse-access/{sub}`（权限 `erp.inventory.manage_access`）授予；一个仓库都没授的用户看不到任何仓库、余额与流水。
- 演示数据（可选）：本组件、`infra/iam-casdoor`、`infra/authz` 都跑起来之后，在组件目录 `make seed`。它经真实接口在两个仓库里入库、调整、预留、出库，其中有几行低于低库存阈值。

## 依赖说明

无。本组件只被调用、自己不调任何人：`erp/sales`（以及移动端 BFF）经 gRPC 与 REST 调它；它从不调用别的组件，所以永远不会处在调用链中间，也不会成环。尤其不依赖 `mdm/product`：产品追踪方式经事件到达，存成摘要副本。

权限 bundle 与 JWKS 从配置里的 URL 拉取，不是依赖边。没有它们时每条受保护路由都失败关闭：没有或无效的 token → `401`；`IAM_JWKS_URL` 为空或不可达 → `403`；还没拉到过 bundle → `503`；合法用户但没有这个权限键 → `403`。`/healthz` 始终 `200`：它只报告本进程活着。

## 配置指南

| 变量 | 含义 |
|---|---|
| `PG_HOST` | PostgreSQL 主机。通常写项目共享值（`$var:PG_HOST`）。 |
| `PG_PORT` | PostgreSQL 端口；默认 `5432`。 |
| `PG_DATABASE` | 放 `erp_inventory` schema 的数据库（`$var:PG_DATABASE`）。 |
| `PG_USER` | 登录角色，字面量 `erp_inventory_rw`。进外壳后外壳用自己的角色登录，每个事务里切到这个角色（`SET LOCAL ROLE`），所以角色名必须是 `<PG_SCHEMA>_rw`。 |
| `PG_PASSWORD` | `PG_USER` 的密码。密钥：写 `${ERP_INVENTORY_DB_PASSWORD}`（或你的密钥库引用），绝不写值。 |
| `PG_SCHEMA` | 全部表、Outbox 与迁移状态表（`schema_migrations_erp_inventory`）所在的 schema；默认 `erp_inventory`。仍然写出字面量，让每个组件的 schema 在一处都看得见。 |
| `NATS_URL` | Outbox 推送与产品事件消费用的 NATS（`$var:NATS_URL`）。 |
| `OTEL_BASE_URL` | OpenTelemetry collector 的基础地址；为空（默认）时什么都不导出。 |
| `AUTHZ_BUNDLE_URL` | 权限判定轮询的 bundle 地址，例如 `http://infra-authz-2-0-0:8223/authz/bundle`。必填：没有它每条受保护路由都回 `503`。要与项目里 authz 的版本保持一致。 |
| `IAM_JWKS_URL` | 本地验签用户 token 的 JWKS 地址，例如 `http://infra-iam-casdoor-2-0-0:8200/.well-known/jwks.json`。必填：没有它每条受保护路由都回 `403`。 |
| `LOW_STOCK_THRESHOLD` | 在 `GET /stats/summary` 里，可用量低于这个数的余额行算低库存。非负十进制数（`10`、`2.5`）；默认 `10`，空值也按默认。其他写法（负数、科学计数法、文字）让组件启动失败，而不是悄悄退回默认值。跟随组件默认就保持注释；启动日志会记下实际生效的值。 |

## 契约索引

- `contracts/erp/inventory/v1/inventory.proto` —— gRPC `erp.inventory.v1.InventoryService`，给别的组件用：`Reserve`、`CancelReservation`、`ConfirmIssue`（都带 `idempotency_key`；一次预留含多个（产品，仓库，数量）项，要么全成要么全不成）、`GetReservationStatus`（按 `reservation_id` 查，或在 `Reserve` 超时时按那次 `Reserve` 的 `idempotency_key` 查；`RESERVATION_STATUS_UNSPECIFIED` 表示查不到，可以安全重试）、`Receive`、`Adjust`、`GetBalance`、`BatchGetBalance`（调用方用它代替循环调 `GetBalance`；缺的行就是 0）、`ListMovements`（游标分页，没有 offset）。Go 包是独立模块 `github.com/brickKit/erp-inventory/gen/erp/inventory`。`Receive`、`Adjust`、`GetBalance`、`ListMovements` 按调用者的仓库授权过滤，这份授权只有验过签的 REST 请求才有；经 gRPC 直连时回 `Internal`。
- `contracts/inventory.openapi.yaml` —— REST，前缀 `/erp/inventory`，每条路由都带权限键：`POST /movements/receive`（`erp.inventory.receive`）、`POST /movements/adjust`（`erp.inventory.adjust`）、`GET /balances`（单条余额；`erp.inventory.view`）、`GET /balances/list`（`erp.inventory.view`；可选 `product_id`、`warehouse_id`，游标分页）、`GET /movements`（`erp.inventory.view`；`created_after` / `created_before`，默认最近 90 天）、`GET /warehouses`（`erp.inventory.view`）、`GET /stats/summary`（`erp.inventory.view`；`sku_count`、`total_on_hand_qty`、最多 50 行的 `low_stock`、`low_stock_count`），以及 `GET` / `POST /warehouse-access/{sub}`、`DELETE /warehouse-access/{sub}/{warehouse_id}`（`erp.inventory.manage_access`）。除仓库授权管理外，每个读写端点都只算授给调用者的仓库。预留相关调用与 `BatchGetBalance` 不在 REST 上。
- `contracts/events/inventory.events.json` —— 经 Outbox 发布：每次入库、出库、调整都发 `erp.inventory.adjusted.v1`（产品、仓库、带符号数量、流水 ID、批次、序列号、原因；`aggregate_id` 是流水 ID，`version` 恒为 1）。`erp.inventory.transferred.v1` 已预留、不发布。消费：`mdm.product.created.v1`、`mdm.product.updated.v1`（只取追踪方式，version 大于本地持有的才应用）。
- `assembly.yaml` —— 本项目的元数据：六个权限键（`erp.inventory.reserve` 与 `.issue` 供角色配置与审计，对应的调用只在 gRPC 上）、菜单项、网关路由 `/erp/inventory/**`、schema 与角色，以及作用在余额、流水、预留三张表上的数据范围 `warehouse`（列 `warehouse_id`，模式 `in`）。

## 外壳声明

不是外壳。它可以由 Go 外壳托管（在 BrickEnterprise 项目里是 `be/go-core`），也可以单独运行；两种方式代码相同。

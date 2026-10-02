[English](AGENTS.md) · [中文](AGENTS.zh.md)

# erp/inventory

开发本组件的 AI 指南。用法、边界与契约：BRICKKIT.md；为什么是这个形状：`docs/design.zh.md`；依赖、配置与部署：component.yaml。

## 代码地图

| 路径 | 负责 |
|---|---|
| `backend/module/module.go` | 唯一入口 `New(ctx, rt)`：读 `PG_SCHEMA` 与 `LOW_STOCK_THRESHOLD`，装配 repo → service → HTTP + gRPC，起四个后台循环（Outbox 推送、周分区与月分区维护、产品事件消费）。单跑与进外壳是同一个函数 |
| `backend/cmd/server/main.go` | 一行 `besdk.RunStandalone(module.New)` |
| `backend/cmd/migrate/main.go` | 一行 `migrate.Main(migrations.FS)`：迁移容器的入口（`./migrate up`） |
| `backend/internal/repo/repo.go` | 哨兵错误、`Repo`、先声明再执行的幂等工具（`claimIdempotency`、`finalizeIdempotency`） |
| `backend/internal/repo/reservation.go` | `Reserve`、`CancelReservation`、`ConfirmIssue`、`GetReservationStatus`：防超卖的条件更新都在这里 |
| `backend/internal/repo/movement.go` | `Receive` 与 `Adjust` 共用一个事务形状（`applyStockChange`）、`erp.inventory.adjusted.v1` 的 Outbox 写入、`ListMovements` |
| `backend/internal/repo/balance.go` | `GetBalance`、`BatchGetBalance`、`ListBalances` |
| `backend/internal/repo/stats.go` | `StockSummary`：SKU 数、在手合计、按阈值找低库存行，按 `numeric` 比较 |
| `backend/internal/repo/warehouse.go` | `ListWarehouses` |
| `backend/internal/repo/access.go` | `warehouse_access`：调用者能看哪些仓库、授予、撤销 |
| `backend/internal/repo/cursor.go` | 列表游标：流水用 `created_at` + `id`，余额用 `id` |
| `backend/internal/repo/snapshot.go` | 产品追踪方式的摘要副本，version 更大才更新 |
| `backend/internal/service/` | 入参校验、调用者可见的仓库（`allowedWarehouseIDs`）、低库存阈值（`stats.go`）、错误 → gRPC status 映射（`status.go`） |
| `backend/internal/http/http.go` | REST 路由，每条都带权限键注册 |
| `backend/internal/grpc/grpc.go` | `erp.inventory.v1.InventoryService` |
| `backend/internal/consumer/consumer.go` | 订阅 `mdm.product.created.v1` / `.updated.v1` |
| `backend/internal/partition/` | `event_outbox` / `event_inbox` 的周分区、`inventory_movements` 的月分区 |
| `migrations/` | SQL 迁移（仓库由 `003_seed_warehouses` 播种），由 `migrations/embed.go` 嵌进二进制 |
| `contracts/` | proto、OpenAPI、事件 schema |
| `gen/erp/inventory/` | 生成的 Go 代码：独立的嵌套 Go 模块，单独打 tag（gen/erp/inventory/v1.x.y）；绝不手改 |
| `scripts/seed.sh` | 经真实接口灌本地演示数据 |

| 要做的事 | 从这里开始 | 然后 |
|---|---|---|
| 能预留 / 出库多少的规则 | `backend/internal/repo/reservation.go`（条件 `UPDATE` 的 `WHERE`） | `backend/internal/repo/repo_test.go`（`TestReserve_并发防超卖`）、`backend/internal/repo/property_test.go` |
| 列表加过滤条件或列 | `backend/internal/repo/balance.go` 或 `backend/internal/repo/movement.go`（静态 SQL 常量） | `backend/internal/http/http.go`、`contracts/inventory.openapi.yaml`、`backend/internal/service/stats_test.go` |
| 仪表盘上的一个数 | `backend/internal/repo/stats.go` | `backend/internal/service/stats.go`、`backend/internal/http/http.go`（`statsSummaryHandler`） |
| 新配置键 | `component.yaml`（`configSchema`） | `backend/module/module.go`、`backend/module/config_test.go`、BRICKKIT.md 的"Configuration" |
| 谁能看哪个仓库 | `backend/internal/repo/access.go` | `backend/internal/service/service.go`（`allowedWarehouseIDs`） |
| 某个错误回错了状态码 | `backend/internal/service/status.go` | `backend/internal/repo/repo.go` 里的哨兵错误 |

## 构建与测试

```bash
# 测试只连测试库 brickkit_test_db，绝不连 brickkit_db（项目根 make test-db-init）
export TEST_PG_DSN="postgres://postgres:<项目 .env 里的 POSTGRES_PASSWORD>@localhost:5432/brickkit_test_db?sslmode=disable"
export TEST_NATS_URL=nats://localhost:4222
make test                    # 每个包都是 "ok"；没设 TEST_PG_DSN 时拒绝运行
go test ./... -count=1 -v | grep -c -- '--- SKIP'   # 0：没有测试被跳过
make check-version dag-check contract-check import-scan module-check   # 各打印一行 ✓
make docs-check              # "0 with errors, 0 warnings"
# 迁移，以登录角色运行（在项目根 make test-db-init ID=erp/inventory 会替你做）：
PG_HOST=localhost PG_PORT=5432 PG_DATABASE=brickkit_test_db PG_USER=erp_inventory_rw \
  PG_PASSWORD=<ERP_INVENTORY_DB_PASSWORD> PG_SCHEMA=erp_inventory make migrate-idempotent   # ✓ 迁移幂等
```

`TestReserve_并发防超卖`（100 个 goroutine 抢 30 件：恰好 30 个成功）是唯一能证明条件更新真的原子的测试；动过 `reservation.go` 的任何地方都要重跑。`buf generate`（契约变了）之后契约包要打新 tag：见易错点。真机验证在项目根：`make verify ID=erp/inventory ROUTE=/erp/inventory/warehouses FOCUS=1`，它构建镜像、只起本组件需要的东西、核对迁移、健康与权限判定、跑一次 focus，然后收尾。

## 设计取舍

- **只被调用，不调任何人。** 没有依赖；产品追踪方式经事件到达。`make dag-check` 遇到任何依赖都失败。
- **防超卖是一条语句**：`UPDATE … WHERE on_hand_qty - reserved_qty >= $qty`；`RowsAffected() == 0` 就是库存不足。`inventory_balances` 上的两条 `CHECK` 是数据库层的兜底。
- **写命令先声明幂等键**（`command_idempotency` 上 `INSERT … ON CONFLICT DO NOTHING`），声明成功才干活；`GetReservationStatus` 按键查时只认 `Reserve` 的键。
- **流水只增不改**；余额是它的投影，在同一个事务里更新。
- **仓库范围来自本组件自己的表**，不来自 token：service 用调用者的 `sub` 查 `warehouse_access`，把列表下推进每条查询的 `WHERE`。
- **列表是静态 SQL**，"参数为空就不限"；余额按 `id` 分页、没有时间窗口，流水按 `created_at` + `id` 分页、默认 90 天窗口。
- **低库存阈值是十进制字符串，在 SQL 里比较**，启动时读一次；非法值让组件启动失败。
- **数量是严格的十进制字符串**：`validateQty` 只认 `^-?[0-9]{1,12}(\.[0-9]{1,6})?$`（与 `NUMERIC(18,6)` 对齐），符号与是否为 0 按字符串判断，从不经过浮点数。

## 易错点

| 不许 | 症状 | 原因 |
|---|---|---|
| 先 `SELECT` 查够不够再 `UPDATE` | 单元测试永远绿，压测偶尔红，生产上一天超卖几单 | 两条语句之间有窗口；条件必须写在 `UPDATE` 的 `WHERE` 里 |
| 给 `dependencies.components` 加一条，尤其是 `mdm/product` | 什么都不坏；枢纽不再是调用图的叶子，下一条边就可能成环 | 调用方预留之前已校验产品；追踪方式经事件到达 |
| 查询之后在 Go 里按可见仓库过滤 | 一页条数不够甚至是空页，`next_cursor` 却还在 | 可见仓库列表必须是 SQL `WHERE` 的一部分 |
| 用 `strconv.ParseFloat` 校验数量 | `"NaN"` 能过、存进 `NUMERIC`，这一行余额从此可以无限超卖；`"Inf"` 是 500 | PostgreSQL 里 `NaN` 排在所有数之上，`CHECK` 与防超卖的 `WHERE` 对它恒真；用 `validateQty` |
| 把空的可见仓库列表当成"不限" | 一个仓库都没授的用户看到全部仓库 | `= ANY('{}')` 什么都匹配不到，这正是想要的失败关闭 |
| 迁移里给 `inventory_movements` 新分区起的名字与 `ensurePartition` 不同（`<表>_YYYY_MM_01`） | 迁移能过；维护循环之后想建一个范围重叠的分区而失败 | `to_regclass` 按这个确切名字查分区 |
| 在 `GetReservationStatus` 里把 `NOT_FOUND` 与 `CANCELLED` 合并 | 上游超时重试时要么重复预留，要么错误地放弃 | `NOT_FOUND` 是请求根本没到（可以安全重试），`CANCELLED` 是到了且已撤销 |
| 给 `inventory_balances` 分区或归档 | 迁移能过；防超卖的 `UPDATE` 要在最忙的表上跨分区找行 | 它的行数上限是产品数 × 仓库数 |
| 改了 `contracts/erp/inventory/v1/inventory.proto` 的注释或选项，却不给契约包打 tag | 本地全绿（`replace` 掩盖了它）；外壳拉 `erp-inventory/v2` 时对着旧的 `gen/` 编译 | `gen/erp/inventory/` 下有任何变化都要打新 tag `gen/erp/inventory/v1.x.y`，根 `go.mod` require 它 |

## 改代码前自查

1. 这件事关于"有多少库存"（这里），还是"为什么要动库存"（订单、采购）？后者绝不放这里。
2. 关于库存的新条件，是不是写在改它的那条 `UPDATE` 的 `WHERE` 里？
3. 每个新的读写是不是都经过 `allowedWarehouseIDs`、并把列表下推进 SQL？
4. 改契约？只增：新字段、rpc、路径、查询参数。绝不删或改类型：字段、rpc、路径、事件 subject 都一样。`gen/` 变了吗？那契约包要打新 tag。
5. 新 REST 路由用 `besdk.GET` / `POST` / `DELETE` 加 `assembly.yaml` 里的权限键注册。
6. 新规则 → 先对着真实测试库写会失败的测试（有范围的用两个用户、两个仓库），再写代码。
7. 发布之后第一次改动前先升 `metadata.version`；BRICKKIT.md 与 `docs/design.md` 与代码同一个提交更新。

[English](design.md) · [中文](design.zh.md)

# erp/inventory 设计

只写结论，给要改本组件设计的人看。怎么用：`BRICKKIT.zh.md`；代码怎么布局：`AGENTS.zh.md`。

## 边界

本组件是"这批货现在还在不在、能不能占"的唯一真相源：余额、库存流水、仓库、预留与防超卖判定。它是项目三个枢纽之一——物理命令枢纽：库存变动的唯一写入方。

| 不在这里 | 归谁 | 为什么 |
|---|---|---|
| 产品是什么（SKU、名称、单位、按批次还是序列号管理） | `mdm/product` | 本组件回答"有多少"，不回答"是什么"。只存一份追踪方式的摘要副本（见事件） |
| 为什么要动库存（销售订单、生产工单） | `erp/sales`、将来的生产组件 | 它收到的是"占 3 件"，不是那张单；`order_id` 只作追溯，从不参与判断 |
| 存货价值、成本方法、会计分录 | `erp/finance` | 数量与金额分家：流水只带数量，财务按 `erp.inventory.adjusted.v1` 自己定价 |
| 采购计划与供应商 | 将来的采购组件 | 本组件只提供 `Receive`，不关心货从哪来 |

## 拥有的数据

| 表 | 分区 | 说明 |
|---|---|---|
| `warehouses` | 否 | 几十行；`code` 唯一；由迁移 `003_seed_warehouses` 播种（`WH-EAST`、`WH-SOUTH`）；没有管理接口 |
| `inventory_balances` | 永不 | 每个（产品，仓库）一行，唯一；`on_hand_qty`、`reserved_qty` 为 `NUMERIC(18,6)`；`CHECK` 两者 ≥ 0 且 `reserved_qty ≤ on_hand_qty`，是数据库层的兜底 |
| `inventory_movements` | 按 `created_at` 月分区 | 只增不改；`qty` 带符号；`reason` 取 `RECEIVE` / `ISSUE` / `ADJUST_GAIN` / `ADJUST_LOSS`；`note`（调整原因）、`order_id`、`batch_no`、`serial_no`；`CHECK serial_no = '' OR abs(qty) = 1` |
| `inventory_reservations` | 否 | 一次 `Reserve` 调用里的每一项一行；`reservation_id`（取自序列）把同一次调用的行归成一组；状态 `RESERVED` → `CONFIRMED` 或 `CANCELLED`，组内所有行在同一个事务里一起迁移 |
| `product_tracking_snapshots` | 否 | `product_id`、`tracking_type`、`version`，来自 `mdm/product` 的事件 |
| `warehouse_access` | 否 | （`sub`，`warehouse_id`）：谁能在哪个仓库看、动库存 |
| `command_idempotency` | 否 | `idempotency_key` 主键、命令及其结果；五个写命令共用 |
| `event_outbox`、`event_inbox` | 按 `created_at` 周分区 | 推送循环从 Outbox 发布，消费者用 Inbox 去重 |

`warehouse_id` 是 schema 内真实的外键；`product_id` 是 `TEXT`、不建外键，是不透明 ID。终态：流水写入即终态；`CONFIRMED` 与 `CANCELLED` 的预留是终态。

**流水只增不改，余额是它的投影**，在同一个事务里更新。纠错是再记一笔；不支持补录过去日期的出入库，因为改写历史就要把之后的每一行重算一遍（ERPNext 的 repost 机制付的代价：advisory lock、检查点、无界级联）。余额与流水对不上时以流水为准。

**防超卖是一条条件更新**：`UPDATE inventory_balances SET reserved_qty = reserved_qty + $1 … WHERE product_id = $2 AND warehouse_id = $3 AND on_hand_qty - reserved_qty >= $1`。`RowsAffected() == 0` 就是库存不足（`FailedPrecondition`）。先 `SELECT` 再写会留下窗口，症状是偶发超卖，单元测试看不出来。`Adjust` 用同样的方式把"不低于 0、不低于已预留量"写进 `WHERE`。（产品，仓库）上的唯一约束让并发的首次入库不会产生重复余额行（Odoo 的 `stock.quant` 没有这条约束，事后再合并重复行）。

## 契约面

gRPC `erp.inventory.v1.InventoryService`：

| rpc | 类型 | 幂等 | 权限键 | 说明 |
|---|---|---|---|---|
| `Reserve` | 写 | `idempotency_key` | `erp.inventory.reserve` | 多项，要么全成要么全不成；销售 TCC 的第一步 |
| `CancelReservation` | 写 | `idempotency_key` | `erp.inventory.reserve` | 补偿动作；查不到、已撤销、已确认都作为状态如实返回，不算错误 |
| `ConfirmIssue` | 写 | `idempotency_key` | `erp.inventory.issue` | 在手与已预留同时减少，一项一条流水；`batch_no` / `serial_no` 对所有项统一生效，需要不同批次 / 序列号的项分成不同的预留 |
| `GetReservationStatus` | 读 | — | `erp.inventory.view` | 按 `reservation_id` 查，或按那次 `Reserve` 的 `idempotency_key` 查（只认 `Reserve` 的键）；返回真正的 `reservation_id` |
| `Receive`、`Adjust` | 写 | `idempotency_key` | `erp.inventory.receive`、`.adjust` | REST 上也有 |
| `GetBalance` | 读 | — | `erp.inventory.view` | 没有余额行就是 0，不是错误 |
| `BatchGetBalance` | 读 | — | `erp.inventory.view` | 调用方用它代替循环调 `GetBalance` |
| `ListMovements` | 读 | — | `erp.inventory.view` | 游标分页，默认 90 天窗口 |

四个预留调用永远不进 REST：它们是组件之间的协议；人类操作是销售侧的出库单。`BatchGetBalance` 也只在 gRPC 上。预留调用的权限键供角色配置与审计，没有 REST 路由检查它们。

**`GetReservationStatus` 为什么存在、为什么必须区分 `NOT_FOUND` 与 `CANCELLED`**：`Reserve` 超时之后，调用方不知道它生效没有。`NOT_FOUND` 表示没生效（可以带同一个键安全重试，先声明再执行的幂等让重试无害）；`CANCELLED` 表示生效了又被撤销。合并成一个"没有"，调用方要么重复预留，要么错误地放弃。`Reserve` 本身超时时调用方手里没有 `reservation_id`，所以可以按那次 `Reserve` 的 `idempotency_key` 查：键 → 预留的映射只在 `Reserve` 提交后才可见，所以"按键查到"等价于"已提交"。

REST，前缀 `/erp/inventory`（全部带权限键）：`POST /movements/receive`、`POST /movements/adjust`、`GET /balances`（单条）、`GET /balances/list`、`GET /movements`（`created_after` / `created_before`，RFC 3339；格式不对 → `400`）、`GET /warehouses`、`GET /stats/summary`，以及仓库授权管理 `GET` / `POST /warehouse-access/{sub}`、`DELETE /warehouse-access/{sub}/{warehouse_id}`（`erp.inventory.manage_access`）。

**给页面与仪表盘的三个读端点**（`erp.inventory.view`，都只算调用者的仓库）：

- `GET /warehouses` → `{ warehouses: [{ id, code, name, status }] }`，按 `id` 排序，不分页：仓库是几十行的播种主数据，同计量单位。没有授权的用户得到空列表。
- `GET /balances/list` → `{ balances: [Balance], next_cursor }`，可选 `product_id`、`warehouse_id`，按余额行 `id` 做 keyset 分页。没有默认时间窗口：余额是当前状态，行数上限是产品数 × 仓库数，很久没动的行也还是库存。点名一个调用者看不到的仓库得到空页（同流水），不是 `403`。`GET /balances` 保持单条语义。
- `GET /stats/summary` → `{ sku_count, total_on_hand_qty, low_stock: [{ product_id, warehouse_id, available_qty }], low_stock_count }`。`sku_count` 数在手 > 0 的不同产品；`total_on_hand_qty` 是在手合计；在手 − 已预留 < `LOW_STOCK_THRESHOLD` 的行算低库存，所以一笔未结的预留能把库存充足的行压到阈值以下。阈值是十进制字符串，在 SQL 里按 `numeric` 比较，从不经过浮点数。清单按可用量排序、最多 50 行；`low_stock_count`（原需求之外加的字段）给出总行数，截断的清单因此看得出是截断的。阈值是配置键而不是一张表：一个部署一个数就够了，直到有人要求按产品设阈值。

## 事件

发布，与流水在同一个事务里写进 Outbox：

| Subject | 何时 | Payload |
|---|---|---|
| `erp.inventory.adjusted.v1` | `Receive`、`ConfirmIssue`（每项一条）、`Adjust` | 产品、仓库、带符号数量、流水 ID、批次、序列号、原因 |
| `erp.inventory.transferred.v1` | — | 为调拨预留，不发布 |

`aggregate_id` 是流水 ID，`version` 恒为 1：每条流水都是一个永不改变的新聚合。`erp/finance` 消费它生成存货凭证；只带数量，财务用自己的成本方法定价。

消费：`mdm.product.created.v1` 与 `mdm.product.updated.v1`，只取 `tracking_type`，写进 `product_tracking_snapshots`。`besdk.Consume` 按（subject，聚合，version）去重，并且每个 subject 只接受更大的 version；两个 subject 之间可能乱序到达，所以 upsert 也只在传入的 version 大于已存的时才生效。

## 依赖

无。预期中有、实际没有的：

| 不是依赖 | 为什么 |
|---|---|
| `mdm/product` | 最诱人的一条边。`product_id` 在这里是不透明的：调用方（`erp/sales`）预留之前已经校验过产品，再校验一遍是多一次往返。追踪方式经事件到达；代价是有一个短窗口里新产品的追踪方式还不知道（它的第一次入库可能漏填必需的批次号，可以人工纠正），而一条同步边会让枢纽永远处在调用链中间 |
| `erp/sales`、`erp/finance` | 本组件被调用、发布事件，不调任何人。财务经事件而不是调用得知数量 |
| `infra/iam-casdoor`、`infra/authz` | token 用 `IAM_JWKS_URL` 本地验签，权限来自 `AUTHZ_BUNDLE_URL` 的 bundle：是配置，不是边 |

## 在同步调用图里的位置

叶子：`erp/sales`（以及将来的采购与生产）和移动端 BFF 调它；它不调任何人，所以永远不会成环。财务只经事件听到它：那条边在事件图里，不在调用图里，两张图不能混着看。库存与财务之间没有任何边；同一个上游（`erp/sales`）并排指挥两者。

## 分区与归档

| 数据 | 热 | 策略 |
|---|---|---|
| `inventory_balances` | 永久 | 永不分区、永不归档：它是写并发最高的表，行数上限是产品数 × 仓库数；分区会让防超卖的 `UPDATE` 跨分区找行 |
| `inventory_movements` | 最近 12 个月 | 按月分区；最初几个月由迁移建，之后由组件自己的维护循环建（当月加往后三个月，每天检查）。两处的分区名都必须是 `<表>_YYYY_MM_01`，即 `ensurePartition` 生成的名字，否则维护循环会想建一个范围重叠的分区。超过 12 个月的整月计划移进 `erp_inventory_archive`；目前还没有代码做分离（见未决问题） |
| `inventory_reservations` | `RESERVED` 的行 | 终态行计划 30 天后删除（流水里有痕迹）；尚未实现 |
| `warehouses`、`product_tracking_snapshots`、`warehouse_access` | 永久 | 小表，不归档 |
| `event_outbox`、`event_inbox` | 周分区 | 由组件提前四周滚动建好 |

## 数据范围

维度 `warehouse`，作用在 `inventory_balances`、`inventory_movements`、`inventory_reservations` 上，列 `warehouse_id`，模式 `in`：分仓管理的客户里，华南仓管理员不该看到华东仓的库存。`warehouse_id` 本来就是业务主键的一部分，这几张表不需要部门或负责人列——那些是 `org` 与 `owner` 两维用的，本组件一维都不用。

谁能看哪个仓库不在 token 里，也不在 `infra/authz` 里：它是拥有仓库的这个组件自己的数据，存在 `warehouse_access`，经仓库授权路由分配。service 用调用者的 `sub` 查出列表，传进每个有范围的读写的 SQL（`= ANY($n)`）；空列表什么都匹配不到。每个有范围的端点都有一条测试：两个用户、两个仓库，断言谁都看不到对方的行。预留调用与 `BatchGetBalance` 不按调用者过滤：它们是组件之间的协议，订单从哪个仓库出货由调用方组件决定。

## 参考实现

| 项目 | 版本 | 看的模块 | 借鉴了什么 | 许可证 | 用法 |
|---|---|---|---|---|---|
| ERPNext | v15 | `erpnext/stock/stock_ledger.py`、`doctype/stock_ledger_entry/` | 流水加余额缓存（`Bin`）的两层结构；以及 repost 机制的代价——这是本组件不支持补录的直接依据 | GPL-3 | 借鉴逻辑 |
| ERPNext | v15 | `doctype/repost_item_valuation/` | 反面教材：允许改写历史换来的锁、检查点与无界级联 | GPL-3 | 借鉴逻辑 |
| Odoo | 17.0 | `addons/stock/models/stock_quant.py` | `available = quantity − reserved`；以及 quant 上没有唯一约束这个反面教训 | LGPL-3 | 借鉴逻辑 |
| Odoo | 17.0 | `addons/stock/models/stock_move.py` | 状态划分；只有 `done` 与 `cancel` 是终态 | LGPL-3 | 借鉴逻辑 |
| Odoo | 17.0 | `addons/stock_account/models/stock_valuation_layer.py` | 数量与价值分层；这里价值整个不在本组件 | LGPL-3 | 借鉴逻辑 |

**刻意避开的**：ERPNext 号称不可变、却被 repost 改写的流水；Odoo 事后合并的重复 quant；Odoo 锁一个序列化令牌而不是锁真正要改的行（这里条件 `UPDATE` 锁的就是它要改的那一行）；"序列号数量为 1"只在代码里管（这里是 `CHECK`）。

**没有参考的**：TCC 式的 `Reserve` / `CancelReservation` / `GetReservationStatus` 在两个系统里都没有对应物（ERPNext 的库存预留没有 TTL、没有幂等键、没有定时释放；Odoo 的预留方式管的是什么时候开始预留，从不管什么时候失效）。它的正确性靠本组件自己的并发测试（`TestReserve_并发防超卖`、`TestProperty_Reserve并发同key仅执行一次`）。

**看到的分歧，不建槽位族**：成本方法（ERPNext 按物料、Odoo 按类别）归财务；补录（ERPNext 改写、Odoo 追加冲销层）已定为"追加"；拣货策略（Odoo 把 FIFO / LIFO / FEFO 参数化）这里只有一种。`erp/sales` 对本组件是必需依赖，所以这些都不能做成槽位；真有客户需要，做成本组件的内部策略或客户 fork。

## 未决问题

| 问题 | 现在的答案 |
|---|---|
| 预留要不要自己过期（TTL）？ | 不要。两个参考系统都没有；做了就要一个扫描任务，外加"预留过期了订单怎么办"的一整套规则。只有上游显式取消才释放库存 |
| 谁来对账余额与流水？ | 计划：定期按（产品，仓库）`SUM(qty)` 与余额比对，不一致就告警并按流水重算。尚未实现 |
| 归档 12 个月以前的流水月份、30 天后删除终态预留 | 策略见上；目前没有代码分离分区或删除预留 |
| `Adjust` 要不要走审批？ | 现在不走：直接生效并记录原因。需要时的接法：本组件在 `infra/workflow` 里发起待办，等它的完成事件再执行调整；workflow 不含业务规则 |
| 序列号追踪的产品：`CHECK` 只能管"填了序列号的行数量是 ±1" | 它看不到追踪方式（在摘要副本里），所以"该填序列号却没填"只能在应用层、在事件窗口之内判断。这个缺口是刻意接受的 |
| 仓库管理 | 迁移播种；有客户需要频繁建仓库时再开管理接口 |
| gRPC 上按调用者的数据范围 | gRPC 调用不带验过签的身份，所以有范围的方法（`Receive`、`Adjust`、`GetBalance`、`ListMovements`）经 gRPC 失败关闭（`Internal`）。经 gRPC 透传用户范围是平台层面的改动（SDK 要在服务端校验转发来的 token） |
| 按产品或按仓库的低库存阈值 | 现在只有一个配置值；有客户要求再加表 |

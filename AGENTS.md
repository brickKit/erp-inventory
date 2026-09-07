# erp-inventory · AI 助手导读

## 身份证

| 项 | 值 |
|---|---|
| 组件 ID | `erp/inventory` |
| 仓库名 | `erp-inventory` |
| 端口 | HTTP `8086` / gRPC `9096`（`registry/ports.tsv`，装配仓库根目录那份） |
| schema / role | `erp_inventory` / `erp_inventory_rw`（归档 schema `erp_inventory_archive`，**本组件真的会用**，见设计计划 §7） |
| 语言 / 框架 | Go：Gin + `database/sql` + `pgx/v5/stdlib` + `sqlc` + `golang-migrate` |
| 合并部署时进 | 外壳一 `go-core` |
| 装配角色 | `default` |
| 设计真相源 | 装配仓库 `docs/design/erp-inventory.md`——本文件与它冲突时，以那份为准，回来改这里 |

## 边界

**归我：** 库存余额、库存流水、库位/仓库、库存预留、防超卖判定——"这批货现在还有没有、能不能占"这个问题的唯一真相源（设计书 §2.6 物理命令枢纽）。

**不归我：**
- 产品是什么（SKU、名称、单位、要不要按批次管）：归 `mdm-product`。本组件只持有 `tracking_type` 的**摘要副本**
- 为什么要出这批货（订单、生产工单）：归 `erp-sales`/`erp-manufacturing`。本组件收到的是"占 N 件"这个命令，不关心背后是哪张单
- 存货的**会计价值**、成本核算：归 `erp-finance`。本组件只发数量变动事件，不算金额

`data_scopes` 声明 `warehouse` 维（设计书 §14.2.2）——分仓管理的客户里，华东仓管理员不该看到华南仓库存明细。`warehouse_id` 本来就是业务主键的一部分，不需要额外的数据权限列。

## 契约面与事件

**gRPC `erp.inventory.v1.InventoryService`：** `Reserve`/`CancelReservation`/`ConfirmIssue`（TCC 三件套，命令）、`Receive`/`Adjust`（入库/调整，命令）、`GetReservationStatus`/`GetBalance`/`BatchGetBalance`/`ListMovements`（读）。`BatchGetBalance` 是防 N+1 的唯一合法调用方式，任何时候都不许删掉它只留 `GetBalance`。

**REST 前缀：** `/erp/inventory/**`，只暴露 `Receive`/`Adjust`/`GetBalance`/`BatchGetBalance`/`ListMovements`。**`Reserve`/`CancelReservation`/`ConfirmIssue`/`GetReservationStatus` 永远不暴露到 REST**——它们是组件间 TCC 协议的一部分，不是人类操作。

**发布事件：** `erp.inventory.adjusted.v1`（`Receive`/`ConfirmIssue`/`Adjust` 都发这条）、`erp.inventory.transferred.v1`（占位，阶段二不实现）。`erp-finance` 消费它生成存货凭证，payload 只带数量不算金额。

**消费事件：** `mdm.product.created.v1`/`.updated.v1`——只取 `tracking_type` 维护摘要副本（`product_tracking_snapshots`）。

## 依赖与「为什么不依赖某某」

`dependencies.components` 永远是空数组。

- **不依赖 `mdm-product`**：反直觉但重要——`product_id` 对本组件是不透明外键，校验产品存不存在是调用方（`erp-sales`）的事，它在调 `Reserve` 之前已经校验过了。`tracking_type` 走事件摘要副本，不走同步调用（设计计划 §5）。
- **不依赖 `erp-sales`/`erp-finance`**：本组件是被调用方与事件发布方，没有任何出边（§2.6）。
- **不依赖 `infra-iam-casdoor`**：IAM 走 JWT 本地验签（决策 87），只需要 `iamJwksUrl` 拉公钥。

## 这个组件特有的坑

| 不许 | 症状 | 出处 |
|---|---|---|
| 给 `dependencies.components` 加任何一条（尤其是 `mdm-product`） | 编译、启动、测试全都正常——**没有任何症状**。但物理命令枢纽从此有了出边，同步图迟早成环 | §2.6、设计计划 §5 |
| 防超卖用"先 SELECT 查够不够，再 UPDATE 扣" | 单元测试永远绿，压测偶尔红，生产上一天错几单——中间有窗口。必须用条件更新（`WHERE on_hand_qty - reserved_qty >= $1`），判定与加锁是同一条语句 | 设计计划 §2.2 |
| `(product_id, warehouse_id)` 上不建唯一约束 | 建表能过、迁移能跑——但并发下会产生重复的余额行（Odoo 的 `stock.quant` 就是这么踩的坑，靠事后合并清理，且清理逻辑本身有 bug） | 设计计划 §2.2、§8 |
| 补录过去日期的出入库，改历史流水行 | ERPNext 的 repost 机制就是这么做的，代价是两把 advisory lock + 检查点文件 + 无界级联。本组件流水真的只增不改，补录用冲销流水 | 设计计划 §2.1 |
| `GetReservationStatus` 把 `NOT_FOUND` 与 `CANCELLED` 合并成一个"没有" | 上游超时重试时会误判——`NOT_FOUND` 说明请求根本没到（能安全重试），`CANCELLED` 说明已被撤销（不能重试）。合并成一个会导致误杀或漏杀 | 设计计划 §3、§4.5 |
| `inventory_balances` 加分区或归档 | §11.2.5 明确注明它必须永远小而快——它是全系统写并发最高的表，分区会让防超卖那条条件更新跨分区找行 | 设计计划 §7 |

## 改代码前的自查

1. **我是不是在给这个组件加一条 `dependencies.components`？** 停下——尤其是 `mdm-product`，几乎总是不必要（见上表第一条）。
2. **我写的这段防超卖逻辑，判定条件是不是写在 SQL 的 `WHERE` 里？** 如果是"先查后写"两条语句，就是错的，无论查得多近。
3. **我是不是在给 `inventory_balances` 加列做分区/归档？** 停下——设计计划 §7 已经判定它永不分区永不归档，除非设计计划本身先改。
4. **这个改动会不会让 `contracts/inventory.proto` 出现破坏性变更？** 下游 `erp-sales`/`erp-purchase`/`erp-manufacturing` 都消费这份契约，只能向后兼容地追加（§3.4 铁律 3）。
5. **我是不是在改 `inventory_movements` 里已经写入的历史行？** 停下——流水只增不改，补录用冲销流水，不改写过去。

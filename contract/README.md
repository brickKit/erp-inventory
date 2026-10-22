# erp-inventory 的契约

`erp/inventory` 发布的契约包：系统面的 protobuf、事件载荷的 JSON Schema、用户面的 OpenAPI。放在组件仓库的 `contract/` 子目录里单独发布（标签 `contract/v3.…`）。

依据：装配项目 [be-assembly-standard](https://github.com/brickKit/be-assembly-standard) 的协议 04（预占分支）、协议 05（不可遗漏的增量事实）、协议 05b 与 06（两个序号）、协议 14（数量、列表）；组件的 `docs/design.md` 与 `docs/rules/availability.md`。

## 内容

| 位置 | 是什么 |
| --- | --- |
| `proto/be/erp/inventory/v3/inventory.proto` | `ReservationService`（协议 04 的参与方命令：`Hold`、`Confirm`、`Cancel`、`Query`、`Adjust`）；`StockService`（`Issue` 出库并消耗分支、`Positions` 余额） |
| `gen/go/be/erp/inventory/v3/` | Go 代码，包 `inventoryv3`；模块 `github.com/brickKit/erp-inventory/contract/v3` |
| `python/` | Python 包 `contract-erp-inventory`，模块 `be.erp.inventory.v3` |
| `events/` | 流 `erp.inventory` 的四种载荷：流水（增量事实）、余额、分支、仓库（完整状态） |
| `openapi.yaml` | 用户面；每个路由的操作写在 `x-operation` |

## 要点

- **数量**是产品基本单位的十进制字符串，任何环节不经浮点。
- **预占分支**：分支身份由协调者生成，每个命令按它幂等；同身份不同内容冲突（RS17）；取消先到留下墓碑，迟到的预占答 `CANCELLED`（RS04）；`Query` 不存在的分支答 `ABSENT`，不留墓碑。全部明细一次占住或全部拒绝，拒绝时 `shortages` 说明每一行。
- **出库** `Issue` 按 `consumption_id` 幂等，在同一个事务里消耗分支（RS19）；任一行不能出就整笔拒绝、什么都不写，`problems` 说明每一行。批次可不指定（按入库先后分配），序列号必须逐个指定。
- **流水事件**是不可遗漏的增量事实：按 `fact_id` 去重；`inventory_seq` 与 `valuation_seq` 写作（纪元，序号），在提交后的编号事务里分配，事件在那里进发件表，所以事件总带着两个序号；用户面在编号之前读到的流水两个序号为 `null`。
- **能力不支持**（D75）：`Adjust`、调拨、调整，答 `CAPABILITY_UNSUPPORTED`。

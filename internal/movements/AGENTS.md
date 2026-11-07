# movements：入库与流水

入库（带批次、序列号或都不带，按产品的追踪政策）与流水的读取。出库的流水由 `issue` 写。

## 负责与不负责

- 负责：表 `movement`（入库的部分）；路由 `/api/inventory/receipts`、`/api/inventory/movements`；追踪政策的检查（`CheckTracking`：按批次的要写批次、在库的序列号不能再入）；服务不能入库；入库后立即编号（`numbering.NumberNow`）。
- 不负责：序号与流水事件（`numbering`）；出库（`issue`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `movements.go` | `receiptLine`、`CheckTracking`、`Receive`、`movements`、`GetMovement`；列表 `movementList`、`ListMovements` |

## 依赖与被依赖

- 依赖：`core`、`masterdata`、`numbering`、`stock`、`warehouses`。
- 被依赖：`issue`（`CheckTracking`）；入口登记路由。

## 规则在哪

`docs/rules/availability.md` 的批次与序列号；装配项目协议 05b、06。

## 测试

`itest/movements_integration_test.go`（`receipts`）。

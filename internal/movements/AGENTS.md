# movements：入库、采购收货与退货、流水

入库（带批次、序列号或都不带，按产品的追踪政策）、采购组件的收货与退货（3.0.37，系统面）与流水的读取。销售出库的流水由 `issue` 写。

## 负责与不负责

- 负责：表 `movement`（入库与采购退货的部分）、`purchase_receipt`、`purchase_return`（按调用的身份记下内容摘要与写出的流水）；采购收货 `ReceivePurchase` 与采购退货 `ReturnPurchase`（退货从原入库流水的仓库与批次出，至多它没退的部分，序列号必须是它收进的、此刻仍在这个仓库）；路由 `/api/inventory/receipts`、`/api/inventory/movements`；追踪政策的检查（`CheckTracking`：按批次的要写批次、在库的序列号不能再入）；服务不能入库；入库后立即编号（`numbering.NumberNow`）。
- 不负责：序号与流水事件（`numbering`）；出库（`issue`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `movements.go` | `receiptLine`、`CheckTracking`、`Receive`、`writeReceipt`（手工入库与采购收货共用）、`movements`、`GetMovement`；列表 `movementList`、`ListMovements` |
| `purchase.go` | `ReceivePurchase`、`ReturnPurchase`、`claim`（调用身份与内容摘要）、`originOf`、`returnSerials` |
| `valuation.go` | `ByValuation`：`StockService.Movements`，成本组件按计价序号补取流水（协议 05b 的缺口），按序回答、带这一对的最大序号 |

## 依赖与被依赖

- 依赖：`core`、`masterdata`、`numbering`、`stock`、`warehouses`。
- 被依赖：`issue`（`CheckTracking`）；入口登记路由。

## 规则在哪

`docs/rules/availability.md` 的批次与序列号；装配项目协议 05b、06。

## 测试

`itest/movements_integration_test.go`（`receipts`）、`itest/purchase_integration_test.go`（`purchases`）；改坏检查在装配项目 `dev/rebuild/c2-data/p0b-inventory-mutants.txt`。

# stock：余额

余额行（产品，仓库）：在库、已预占、可用；批次与序列号。余额行是唯一的裁决点。

## 负责与不负责

- 负责：表 `stock_position`、`stock_lot`、`stock_serial`；多行按（产品，仓库）升序加锁（`LockPositions`，RS16 不死锁）、变动（`MoveStock`）；事件 `erp.stock_position.changed.v1`；路由 `/api/inventory/positions`、`/lots`、`/serials`；`StockService.Positions`。
- 不负责：什么时候预占、出库（`branches`、`issue`）；入库（`movements`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `stock.go` | `Key`、`LockPositions`、`MoveStock`、`Neg`、`AtLeast`；列表 `positionList`、`lotList`、`serialList`、`ListPositions`、`lotOut`、`ListLots`、`serialOut`、`ListSerials`；`Positions` |

## 依赖与被依赖

- 依赖：`core`。
- 被依赖：`movements`、`branches`、`issue`；入口。

## 规则在哪

`docs/rules/availability.md`（每个命令改哪几行、加什么锁）；“设计取舍”（`AGENTS.md`，仓库根）。

## 测试

`itest/branches_integration_test.go`（`racingHolds`：不超卖、不死锁）、`itest/movements_integration_test.go`、`itest/issue_integration_test.go`、`itest/dimensions_integration_test.go`。

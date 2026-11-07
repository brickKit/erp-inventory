# warehouses：仓库

每个法人的仓库：代码、名称、状态。

## 负责与不负责

- 负责：表 `warehouse`；路由 `/api/inventory/warehouses` 的列表、建立、读取、修改；事件 `erp.warehouse.changed.v1`；代码在一个法人里只能用一次；法人要能用；`UsableWarehouses`（一组仓库是否都属于这个法人且启用）。
- 不负责：余额（`stock`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `warehouses.go` | `codeForm`、`CreateWarehouse`、`Warehouse`、`GetWarehouse`、`UpdateWarehouse`、`UsableWarehouses`；列表 `warehouseList`、`ListWarehouses` |

## 依赖与被依赖

- 依赖：`core`、`masterdata`。
- 被依赖：`movements`、`branches`；入口登记路由。

## 规则在哪

`docs/design.md`；安全 07（法人与仓库维度）。

## 测试

`itest/warehouses_integration_test.go`（`warehouses`）、`itest/dimensions_integration_test.go`。

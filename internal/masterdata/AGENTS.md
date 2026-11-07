# masterdata：向主数据取证（共用）

向 `mdm/org`（法人）与 `mdm/product`（产品：种类、基本单位、追踪政策、状态）取证。每个调用都在事务之外：先取证，再开事务。

## 负责与不负责

- 负责：`LegalEntityOf`、`Product`、`ProductsOf`。
- 不负责：仓库、余额（各模块）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `masterdata.go` | `legalEntity`、`LegalEntityOf`、`Product`、`ProductsOf` |

## 依赖与被依赖

- 依赖：`core`。
- 被依赖：`warehouses`、`movements`、`branches`、`issue`。

## 规则在哪

装配项目协议 07（取证在事务外）。

## 测试

由流程测试间接覆盖；替身在 `itest/endtoend_integration_test.go`。

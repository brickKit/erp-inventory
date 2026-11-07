# core：全组件共用的名字与写法（共用）

每个模块都引用、自己什么业务也不做的东西。改这里会影响所有模块。

## 负责与不负责

- 负责：组件 ID、依赖的 ID、流与事件类型、资源与操作、法人与仓库两个维度、`MaxBatch`、暂存期限、资源定义 `Definitions()`（`names.go`）；本组件的错误原因、身份的格式、本版不做的路由、数据范围与授权检查、校验、数量（`Quantity`）、审计（`Change`）、列表的 `Filters`、`Build`（`rules.go`）；记录的类型、读取、发件表（`Emit`）（`records.go`）；`Base`（`base.go`）。
- 不负责：任何业务规则（在各模块）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `base.go` | `Base`：组件的运行时 |
| `names.go` | 常量；`fields`、`dims`、`Definitions()` |
| `rules.go` | 错误原因、`UUIDForm`、`Unsupported`、`Scope`、`Check`、`Invalid`、`NotFound`、`Quantity`、`Change`、`Filters`、`Build` |
| `records.go` | `Warehouse`、`Position`、`Movement`、`Branch` 等记录与 `Scan…`、`Get…`、`One`；`Emit` |

## 依赖与被依赖

- 依赖：只有 SDK 与契约。
- 被依赖：全部模块与入口。

## 规则在哪

`docs/design.md`；装配项目协议 04、05、05b、06、14，安全 07。不用浮点算数量。

## 测试

由 `itest/` 的流程测试间接覆盖。

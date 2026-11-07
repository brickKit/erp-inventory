# inventory/：入口与组装

组件对外的一个包：`main.go` 与外壳只引用它（`inventory.Definition()`、`inventory.New`）。这里只**组装**：把 `internal/` 下模块的函数登记成 SDK 要的路由、gRPC 方法、后台工作与恢复核对；业务一行也不写。

## 负责与不负责

- 负责：组件的版本、端口、迁移、组件定义；哪个路由、哪个 gRPC 方法、哪个后台工作交给哪个模块；契约里有、本版不做的路由（调拨、盘点调整，交给 `core.Unsupported`）。
- 不负责：业务规则（去 [internal/AGENTS.md](../internal/AGENTS.md) 找模块）；资源、操作、维度、事件类型的名字（`internal/core/names.go`）；集成测试（`itest/`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `definition.go` | `Version`、`GRPCPort`、迁移；`Definition()`：身份、依赖、流、资源与操作（`core.Definitions()`） |
| `module.go` | `Module` 与 `New`；`Routes`、`Services`（预占分支与出库只许 `erp/sales`，余额任何组件）、`Jobs`（流水编号 → `numbering.Number`，暂存到期 → `branches.ExpireHolds`）；`handler` 适配 |
| `services.go` | gRPC 方法 `Hold`、`Confirm`、`Cancel`、`Query`、`Adjust` 转给 `internal/branches`，`Issue` 转给 `internal/issue`，`Positions` 转给 `internal/stock` |
| `checks.go` | 整套恢复的核对：仓库、流水等按版本答“还在不在” |
| `contract_test.go` | 单元测试：清单的 `configSchema` 与 `events`、OpenAPI 与路由一一对应、事件载荷合乎 schema、合并安全 |
| `structure_test.go` | 单元测试：`internal/` 里没有写 SDK 分支表（`be_reserve_branch`、`be_reserve_consumption`）的 SQL |
| `migrations/` | 表 |

## 依赖与被依赖

- 依赖：`internal/core`、`warehouses`、`stock`、`movements`、`numbering`、`branches`、`issue`。
- 被依赖：`main.go`、`cmd/seed`、外壳 `be/go-core`、`itest/`。

## 测试

`go test ./inventory/`。

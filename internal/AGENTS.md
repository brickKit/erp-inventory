# internal/：业务模块

库存的业务，每个模块一个包。只给本仓库引用（Go 的 `internal`）：别的组件编译期就引用不了，跨组件只走契约（决定 0101）。先按下表找到模块，再读那个目录的 `AGENTS.md`。

## 模块

| 包 | 一句话 | 依赖（本组件内） |
| --- | --- | --- |
| `warehouses` | 仓库 | `core`、`masterdata` |
| `stock` | 余额的加锁与变动、批次、序列号、余额的读取 | `core` |
| `movements` | 入库、采购收货与退货、流水的读取 | `core`、`masterdata`、`numbering`、`stock`、`warehouses` |
| `numbering` | 两个序号的提交后编号与流水事件 | `core` |
| `branches` | 预占分支（状态机在 SDK 的 `reserve`） | `core`、`masterdata`、`stock`、`warehouses` |
| `issue` | 出库并消耗分支、批次分配 | `branches`、`core`、`masterdata`、`movements`、`numbering`、`stock` |
| `masterdata`（共用） | 事务外向 `mdm/org`、`mdm/product` 取证 | `core` |
| `core`（共用） | 名字与资源定义、校验、审计与事件、记录、列表的共用部分、`Base` | — |

分层（每个包只引用比它低的层）：`issue` → `branches`、`movements` → `warehouses`、`numbering`、`stock` → `masterdata` → `core`。

## 共同的写法

- 每个模块的函数先收 `m *core.Base`（组件的运行时），入口包的 `Module` 持有它、登记时适配。
- 余额行是唯一的裁决点：在库与已预占同在一行，多行按（产品，仓库）升序加锁（`stock.LockPositions`），变动只经 `stock.MoveStock`；库里 `on_hand >= reserved` 的约束兜底。
- 事务外取证、事务内写：主数据的调用都在 `masterdata`，在事务之前；写入、审计（`core.Change`）与发件表（`core.Emit`）同一个事务；序号在提交之后由 `numbering` 编。

## 测试

模块的业务规则由 `itest/` 的流程测试覆盖。

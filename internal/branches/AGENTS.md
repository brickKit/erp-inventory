# branches：预占分支

装配项目协议 04 的参与方：`erp/sales` 的确认过程在这里预占实物分支。预占全有或全无；暂存到期释放；先取消后预占留下墓碑。**分支的状态机在 SDK**（be-sdk-go 的 `reserve`，线协议第 16 章）：状态与迁移、按分支身份与内容摘要幂等、墓碑、暂存期限与到期、按消耗身份幂等、先锁余额再锁分支都在那里；这里只写占什么、退回什么。

## 负责与不负责

- 负责：参与的种类 `Kind`（`erp.inventory.order`）与它的三个函数（按“产品 + 仓库”的顺序锁明细的余额行、退回未消耗的部分、发 `erp.inventory_branch.changed.v1`）；`Participant` 给出库用；表 `branch`（法人、引用、拒绝时的缺货明细）、`branch_line`（明细与已消耗）；`ReservationService` 的 `Hold`（检查产品与仓库、锁余额、全部占住或全部拒绝）、`Confirm`、`Cancel`、`Query`、`Adjust`（本版不做）；暂存到期的工作项（`reserve.ExpireDue`）；用户面读一个分支（按维度）；预占与释放改已预占（经 `stock.MoveStock`）。
- 不负责：分支的状态与它的迁移（SDK 的 `reserve`，组件不写 `be_reserve_branch`，`inventory/structure_test.go` 检查）；出库消耗（`issue`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `branches.go` | `BranchStates`、`wire`、`absent`、`BranchID`、`holdContent`；`Hold`；`Kind`、`kind`、`Participant`、`keysOf`、`answered`、`onBranch`；`Confirm`、`Cancel`、`Query`、`Adjust`；`ExpireHolds`；`GetBranchHTTP` |

## 依赖与被依赖

- 依赖：`core`、`masterdata`、`stock`、`warehouses`；SDK 的 `reserve`。
- 被依赖：`issue`（`Participant`、`BranchStates`）；入口。

## 规则在哪

线协议第 16 章（P16.1–P16.13）；`docs/rules/availability.md`；装配项目协议 04（RS 编号）。

## 测试

`itest/branches_integration_test.go`（`racingHolds`、`holdIdempotence`、`lateAndExpired`）、`itest/participant_integration_test.go`（`participant`：业务流程走不到的格）。状态机本身的测试在 SDK（`reserve` 的向量与集成测试）。

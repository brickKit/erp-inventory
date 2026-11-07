# branches：预占分支

装配项目协议 04 的参与方：`erp/sales` 的确认过程在这里预占实物分支。预占全有或全无；暂存到期释放；先取消后预占留下墓碑。

## 负责与不负责

- 负责：表 `branch`、`branch_line`；`ReservationService` 的 `Hold`、`Confirm`、`Cancel`、`Query`、`Adjust`（本版不做）；暂存到期的工作项；用户面读一个分支（按维度）；事件 `erp.inventory_branch.changed.v1`；预占与释放改已预占（经 `stock.MoveStock`）。
- 不负责：出库消耗（`issue`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `branches.go` | `BranchStates`、`wire`、`absent`、`BranchID`、`holdContent`；`Hold`；`release`、`SetState`、`expired`；`Confirm`、`Cancel`、`Query`、`Adjust`、`onBranch`；`ExpireHolds`；`GetBranchHTTP` |

## 依赖与被依赖

- 依赖：`core`、`masterdata`、`stock`、`warehouses`。
- 被依赖：`issue`；入口。

## 规则在哪

`docs/rules/availability.md`；装配项目协议 04（RS 编号）。

## 测试

`itest/branches_integration_test.go`（`racingHolds`、`holdIdempotence`、`lateAndExpired`）。

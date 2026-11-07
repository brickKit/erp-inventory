# issue：出库

带分支的出库：在同一个事务里消耗已确认的分支（RS19，消耗身份就是交货身份）、减在库与已预占（可用量不变）、写流水；按消耗身份幂等；整笔拒绝（不部分出库）。批次按先进先出分配，序列号要点名。

## 负责与不负责

- 负责：表 `consumption`；`StockService.Issue`；`allocateBatches`。
- 不负责：预占（`branches`）；序号（`numbering`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `issue.go` | `refused`、`Error`、`allocation`、`Issue`、`allocateBatches` |

## 依赖与被依赖

- 依赖：`branches`、`core`、`masterdata`、`movements`、`numbering`、`stock`。
- 被依赖：入口 `inventory/services.go`。

## 规则在哪

`docs/rules/availability.md` 的批次与序列号；装配项目协议 04 的 RS19。

## 测试

`itest/issue_integration_test.go`（`issue`、`serialIssue`）。

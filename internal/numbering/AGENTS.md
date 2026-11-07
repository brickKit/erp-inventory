# numbering：两个序号

流水的库存序号与计价序号在提交之后编（出入库的事务不碰全局计数，热点只在编号事务里）；流水事件在计价编号的事务里写，所以总带着两个序号。提交后立刻编一次（`NumberNow`），失败由每秒一次的单例工作项（`Number`）补上。

## 负责与不负责

- 负责：表 `seq_counter`；`movement` 的两个序号列；事件 `erp.inventory_movement.recorded.v1`。
- 不负责：流水怎样产生（`movements`、`issue`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `numbering.go` | `numberLimits`、`numberBatch`、`counter`、`numberInventory`、`numberValuation`、`Number`、`NumberNow` |

## 依赖与被依赖

- 依赖：`core`。
- 被依赖：`movements`、`issue`；入口的 `Jobs`。

## 规则在哪

装配项目协议 05b、06（流水与两个序号）。

## 测试

`itest/movements_integration_test.go`（`numbering`：没有缺号、按产品与法人计价）。

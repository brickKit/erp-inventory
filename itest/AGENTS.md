# itest/：集成测试与测试索引

库存组件对着真实的 PostgreSQL 与消息系统、经它真实的接口（用户面带 be-sdk-go `sectest` 的令牌；系统面走 mTLS，测试以 `erp/sales` 的身份调用（采购收货与退货以 `erp/purchase`））的测试（L2）。`mdm/org`、`mdm/product` 是按契约写的替身。构建标签 `integration`；一条业务流程一个文件。

## 怎样跑

- `make test-integration`（`go test -count=1 -tags integration ./itest/`），约 3 秒。
- 环境变量：`BE_SDK_TEST_PG_ADMIN=postgres://postgres:<口令>@localhost:5432/brickkit_test_db`（口令是装配项目 `.env` 的 `POSTGRES_PASSWORD`）、`BE_SDK_TEST_NATS_URL=nats://localhost:4222`。
- 前提：装配项目里 `make up` 起了 `be-postgres` 与 `be-nats`；`be-postgres` 里有库 `brickkit_test_db`；schema `erp_inventory_it` 每次重建；端口 28086、9096 与替身的 19097、19098 要空着。
- 授权视图是定期取的：测试中途加的授予要等下一次取到（用 `Eventually`）。
- **只有一个测试** `TestInventoryEndToEnd`：按下表的顺序跑各步（普通的方法调用），后面的步用前面建的仓库、库存与分支，所以一步不能单独跑。

## 文件

按运行顺序：

| 文件 | 步 | 测的流程 | 被测的模块 |
| --- | --- | --- | --- |
| `warehouses_integration_test.go` | `warehouses` | 一个法人里代码只能用一次；不认识的法人被拒 | `warehouses` |
| `movements_integration_test.go` | `receipts` | 批次、序列号、普通的入库；追踪政策决定一行要写什么；服务不入库 | `movements`、`stock` |
| `branches_integration_test.go` | `racingHolds`、`holdIdempotence`、`lateAndExpired` | RS01 不超卖、RS16 不死锁且全有或全无；RS03 应答丢了再预占只占一次、RS17 内容不同被拒、别的法人的仓库是无效的行；RS04 墓碑拒绝迟到的预占、不认识的分支答不存在、过期的暂存不复活并退回库存 | `branches`、`stock` |
| `issue_integration_test.go` | `issue`、`serialIssue` | RS19 出库在同一事务里消耗分支、同一交货再出不多出、先进先出；序列号要点名，出一半分支继续 | `issue` |
| `movements_integration_test.go` | `numbering` | 流水事件带两个序号；库存序号没有缺号；计价序号按产品与法人计 | `numbering` |
| `purchase_integration_test.go` | `purchases` | 采购收货（3.0.37）：只许采购组件、同一收货再来不多收、内容不同被拒、流水带收货行；采购退货：从原入库流水的仓库与批次出、至多它没退的、序列号必须是它收进的、不能从退货再退（另收一批让可用量充足，每个拒绝都是它自己的规则） | `movements` |
| `dimensions_integration_test.go` | `dimensions` | Bob 只有北京仓：余额、分支、仓库只看得到能看的 | `stock`、`branches`、`warehouses` |
| `recoverycheck_integration_test.go` | `recoveryChecks` | 整套恢复的核对：仓库与流水按版本答“还在不在” | 入口 `inventory/checks.go` |
| `participant_integration_test.go` | `participant` | 协议 04 的分支迁移里业务流程走不到的格（用自己收的货）：已结束的分支再确认、再取消不变；只从已确认的分支出库（拒绝是出库的问题，不是错误）、不超出剩余、出完成为已消耗；消耗身份换内容是冲突；查询不留墓碑；到期作业让过期的暂存到期；调整答“不支持” | `branches`、`issue` |

共用的：

| 文件 | 内容 |
| --- | --- |
| `endtoend_integration_test.go` | `TestInventoryEndToEnd`（各步的顺序）；两个替身；`fixture`（运行中的成员、系统面的两个客户端、法人、产品、仓库、步与步之间传的分支）；`hold`、`line`、`position`；`start`；`client.do`、`serve`、`portFree` |

## 加测试

新流程加一个文件与一步，在 `TestInventoryEndToEnd` 里按依赖排进顺序，在上表加一行；测试不为了通过而改。

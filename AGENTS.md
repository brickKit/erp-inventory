# erp/inventory

库存组件的代码。用法与边界在 `BRICKKIT.md`，为什么这样设计（术语、数据、规则、未决问题）在 [docs/design.md](docs/design.md)，可用量与预占分支的细则（每个命令改哪几行、加什么锁、什么时候拒绝）在 [docs/rules/availability.md](docs/rules/availability.md)；跨组件的规则在装配项目 be-assembly-standard 的协议 04（预占分支，本组件是参与方）、协议 05 与 05b、06（流水与两个序号）、协议 14（数量、列表）、安全 07（维度）。

## 代码地图

代码按业务模块分层。**先读这一节，再进目录**：每个代码目录都有自己的 `AGENTS.md`（同目录的 `CLAUDE.md` 让 Claude Code 读这个目录的文件时自动带上它），写这一层负责什么、不负责什么、每个文件、依赖谁、测在哪。

| 目录 | 是什么 |
| --- | --- |
| [`inventory/`](inventory/AGENTS.md) | 入口：组件定义与组装（路由、系统面、后台工作、恢复核对都在这里登记，转给业务模块）；不写业务。`main.go` 与外壳只引用它 |
| [`internal/`](internal/AGENTS.md) | 业务：下面的模块各一个包；模块之间的依赖表在它的 `AGENTS.md` |
| [`itest/`](itest/AGENTS.md) | 集成测试，按业务流程分文件；它的 `AGENTS.md` 是测试索引 |
| `inventory/migrations/` | 表 |
| `cmd/seed/` | 种子：角色与授予、四个法人的仓库（含停用的反例）、每个仓库的期初入库（批次、序列号、库存不足的反例）（`make seed`） |
| `contract/` | 本组件发布的契约，单独的模块：`contract/proto/`、`contract/events/`、`contract/openapi.yaml` |
| `docs/` | 设计与细则 |

业务模块（`internal/` 下）：

| 模块 | 负责 | 拥有的表 |
| --- | --- | --- |
| [`warehouses`](internal/warehouses/AGENTS.md) | 仓库：建立、修改、读取、列表；能不能用 | `warehouse` |
| [`stock`](internal/stock/AGENTS.md) | 余额：加锁顺序与变动（在库、已预占）、批次与序列号、余额与批次与序列号的列表、系统面的 `Positions` | `stock_position`、`stock_lot`、`stock_serial` |
| [`movements`](internal/movements/AGENTS.md) | 入库、追踪政策的检查、流水的读取与列表 | `movement` |
| [`numbering`](internal/numbering/AGENTS.md) | 两个序号的提交后编号与流水事件 | `seq_counter` |
| [`branches`](internal/branches/AGENTS.md) | 预占分支（协议 04，本组件是参与方）：预占、确认、取消、查询、暂存到期 | `branch`、`branch_line` |
| [`issue`](internal/issue/AGENTS.md) | 出库并消耗分支、批次的先进先出分配 | `consumption` |

共用的：[`masterdata`](internal/masterdata/AGENTS.md)（事务外向 `mdm/org`、`mdm/product` 取证）、[`core`](internal/core/AGENTS.md)（名字与资源定义、校验、审计与事件的写法、记录的类型与读取、列表的共用部分）。

| 想改 | 从这里开始 | 然后 |
| --- | --- | --- |
| 可用量、预占、取消的规则 | `docs/rules/availability.md` 的表 | `internal/branches`；`itest/branches_integration_test.go` |
| 出库的规则（批次、序列号、整笔拒绝） | `docs/rules/availability.md` 的批次与序列号 | `internal/issue`；`itest/issue_integration_test.go` |
| 分配策略（先进先出之外的，例如按到期） | `internal/issue` 的 `allocateBatches` | 设计的“部署与替换” |
| 序号与流水事件 | `internal/numbering` | 协议 05b、06；`itest/movements_integration_test.go` 的 `numbering` |
| 一个维度、一个操作 | `internal/core/names.go` | `docs/design.md` 的命令与查询、`BRICKKIT.md` 的契约索引；路由的登记在 `inventory/module.go` |
| 加字段、加接口、改事件 | 装配项目 be-assembly-standard 规范里的“改动配方” | `docs/design.md` 的数据与命令 |

## 构建与测试

- `go test ./...`（`make test`）：清单的 `configSchema` 与 `events`、OpenAPI 与路由、事件载荷、合并安全。
- `make test-integration`（`go test -tags integration ./itest/`）：真实的 PostgreSQL 与消息系统；每个文件测哪条流程、要什么前提，在 [itest/AGENTS.md](itest/AGENTS.md)。
- `brickkit lint`

## 设计取舍

- **余额行是唯一的裁决点**：在库与已预占在同一行，库里还有 `on_hand >= reserved` 的约束兜底；多行按（产品，仓库）升序加锁，不死锁（RS16）。
- **出库同时减在库与已预占**：带分支的出库发出的是占住的货，可用量不变。
- **序号在提交之后编**：出入库的事务不碰全局计数，热点只在编号事务里；流水事件在计价编号的事务里写，所以总带着两个序号。提交后立刻编一次，失败由每秒一次的单例工作项补上。
- **预占的幂等靠分支身份，出库的幂等靠消耗身份**：系统面的这两个命令不用请求幂等键，身份就是业务的。

## 易错点

- 不要在事务里调 `mdm/product` 或 `mdm/org`：先取（`internal/masterdata`），再开事务。
- 列表能排序、筛选的字段必须在资源定义的字段里声明（`internal/core/names.go`），否则 SDK 答 `FIELD_NOT_QUERYABLE`。
- 不要用浮点算数量：比较与加减在 SQL 里做（`stock.AtLeast`、`trim_scale`）。

## 改代码前自查

- 改了契约（`contract/`）：proto 改后 `make -C contract gen`，发布 `contract/v…`，再改这里的依赖；OpenAPI 与路由由测试核对。
- 改了版本号吗？每次改动都升版本。

<!-- brickkit:managed:begin lang=zh -->
## BrickKit

这是一个 BrickKit 组件：平台只读 `component.yaml`。它依赖的规则：

- `configSchema` 的键就是代码读的环境变量名。不能用保留名：`COMPONENT_ID`、`COMPONENT_VERSION`、`PORT`、`BRICKKIT_SERVED_MEMBERS`、`BRICKKIT_SERVED_MEMBERS_CONFIG`，以及任何 `*_ENDPOINT`。
- 依赖写精确版本。依赖的地址以 `<ID>_ENDPOINT` 注入；缺席的可选依赖根本没有这个变量，读的时候要带兜底。
- `/healthz` 只查本进程，不查依赖。迁移命令用同一个镜像跑，遇到不认识的参数必须直接失败；`local.migrationCommand` 从源码跑同一条迁移，给 `mode: local` 用。
- `BRICKKIT.md` 会随版本进入每个使用它的项目，在那里是脱离仓库单独读的：跟代码一起改，不放相对链接。
- 发版：改 `metadata.version`，提交、推送，`brickkit release`。`brickkit lint` 会检查清单和这些文档——在项目里、在这个目录下跑，只查这个组件（`--all` 查整个项目）。
- 完整规则在 `brickkit-component` 技能里（装了技能的项目或仓库根目录下的 `.claude/skills/brickkit-component/SKILL.md`；`brickkit skills update` 会装上）；参数问 `brickkit <命令> --help`；BrickKit 自己的文档用 `brickkit docs` 看。
<!-- brickkit:managed:end -->

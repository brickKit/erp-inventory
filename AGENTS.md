# erp/inventory

库存组件的代码。用法与边界在 `BRICKKIT.md`，为什么这样设计（术语、数据、规则、未决问题）在 [docs/design.md](docs/design.md)，可用量与预占分支的细则（每个命令改哪几行、加什么锁、什么时候拒绝）在 [docs/rules/availability.md](docs/rules/availability.md)；跨组件的规则在装配项目 be-assembly-standard 的协议 04（预占分支，本组件是参与方）、协议 05 与 05b、06（流水与两个序号）、协议 14（数量、列表）、安全 07（维度）。

## 代码地图

| 位置 | 内容 |
| --- | --- |
| `inventory/definition.go` | 组件定义：身份、依赖、流与事件类型、资源与操作、法人与仓库两个维度、暂存期限 |
| `inventory/module.go` | 路由与系统面的注册、本组件的错误原因、取法人与产品（事务外） |
| `inventory/warehouses.go` | 仓库的建立、修改、读取；`usableWarehouses` |
| `inventory/stock.go` | 余额行的加锁顺序（`lockPositions`）与变动（`moveStock`） |
| `inventory/receipts.go` | 入库、追踪政策的检查（`checkTracking`）、流水的读取 |
| `inventory/branches.go` | 预占分支：`Hold`、`Confirm`、`Cancel`、`Query`，释放（`release`），暂存到期的工作项 |
| `inventory/issue.go` | 出库并消耗分支（`Issue`），批次的先进先出分配（`allocateBatches`） |
| `inventory/numbering.go` | 两个序号的提交后编号与流水事件；后台工作的声明（`Jobs`） |
| `inventory/lists.go` | 用户面的列表；系统面的 `Positions` |
| `inventory/records.go` | 记录的类型、读取与事件 |
| `inventory/migrations/` | 表 |
| `contract/` | 本组件发布的契约，单独的模块：`contract/proto/`、`contract/events/`、`contract/openapi.yaml` |
| `docs/` | 设计与细则 |

| 想改 | 从这里开始 | 然后 |
| --- | --- | --- |
| 可用量、预占、取消的规则 | `docs/rules/availability.md` 的表 | `branches.go`、集成测试的预占一段 |
| 出库的规则（批次、序列号、整笔拒绝） | `docs/rules/availability.md` 的批次与序列号 | `issue.go` |
| 分配策略（先进先出之外的，例如按到期） | `issue.go` 的 `allocateBatches` | 设计的“部署与替换” |
| 序号与流水事件 | `numbering.go` | 协议 05b、06 |
| 一个维度、一个操作 | `definition.go` | `docs/design.md` 的命令与查询、`BRICKKIT.md` 的契约索引 |
| 加字段、加接口、改事件 | 装配项目 be-assembly-standard 规范里的“改动配方” | `docs/design.md` 的数据与命令 |

## 构建与测试

- `go test ./...`（`make test`）：清单的 `configSchema` 与 `events`、OpenAPI 与路由、事件载荷、合并安全。
- `go test -tags integration ./inventory/`（`make test-integration`）：真实的 PostgreSQL 与消息系统，例如 `BE_SDK_TEST_PG_ADMIN=postgres://postgres:<口令>@localhost:5432/brickkit_test_db`（口令是装配项目 `.env` 的 `POSTGRES_PASSWORD`）与 `BE_SDK_TEST_NATS_URL=nats://localhost:4222`；be-sdk-go 的 `sectest` 是测试部署；`mdm/org` 与 `mdm/product` 是按契约写的替身，测试自己以 `erp/sales` 的身份调用系统面。
- 集成测试的前提：装配项目里 `make up` 起了 `be-postgres` 与 `be-nats`；`be-postgres` 里有库 `brickkit_test_db`（没有就 `docker exec be-postgres createdb -U postgres brickkit_test_db`）；测试每次开始时删掉再建自己的 schema `erp_inventory_it`，跑完留着；端口 28086、9096 与替身的 19097、19098 要空着，被占时测试直接失败。
- 授权视图是定期取的：测试中途加的授予要等下一次取到（集成测试用 `Eventually`）。
- `brickkit lint`

## 设计取舍

- **余额行是唯一的裁决点**：在库与已预占在同一行，库里还有 `on_hand >= reserved` 的约束兜底；多行按（产品，仓库）升序加锁，不死锁（RS16）。
- **出库同时减在库与已预占**：带分支的出库发出的是占住的货，可用量不变。
- **序号在提交之后编**：出入库的事务不碰全局计数，热点只在编号事务里；流水事件在计价编号的事务里写，所以总带着两个序号。提交后立刻编一次，失败由每秒一次的单例工作项补上。
- **预占的幂等靠分支身份，出库的幂等靠消耗身份**：系统面的这两个命令不用请求幂等键，身份就是业务的。

## 易错点

- 不要在事务里调 `mdm/product` 或 `mdm/org`：先取（`productsOf`、`legalEntityOf`），再开事务。
- 列表能排序、筛选的字段必须在资源定义的字段里声明（`definition.go`），否则 SDK 答 `FIELD_NOT_QUERYABLE`。
- 不要用浮点算数量：比较与加减在 SQL 里做（`atLeast`、`trim_scale`）。

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

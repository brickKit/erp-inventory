# erp/inventory

## 组件定位

库存：物理库存变动的唯一写入者。有实物库存的客户需要；只有服务订单的客户可以不装。
- **仓库**：属于一个法人，代码在法人内唯一；停用的仓库不再收货、不再预占。
- **流水**：入库与出库，不可修改（更正以反向流水表达）。每条流水在提交之后得到两个连续序号：库存流水序号（整个库存范围，成本组件关账时取高点与补取）与计价序号（每个“产品 + 法人”，成本组件按它计价）；流水事件在编号之后发出，总带着两个序号。
- **余额**：每个“产品 + 仓库”的在库、已预占与可用（可用 = 在库 − 已预占 ≥ 0，不超卖）；批次追踪产品另有每个批次的在库，序列号追踪产品另有每个序列号的所在与是否在库。
- **预占分支**（协议 04 的参与方）：协调者（`erp/sales`）为一张订单的一次确认预占、确认、取消、查询；全部明细一次占住或全部拒绝；取消先到留下墓碑，迟到的预占被拒；暂存 5 分钟，已到期的不能确认复活；确认占用没有自动到期。
- **出库**：带分支身份，在同一个事务里消耗分支，按消耗身份幂等；批次可以不指定（按入库先后分配），序列号必须逐个指定。任一行出不了就整笔拒绝。

存货成本不在这里（`erp/costing`），订单与交货的业务状态不在这里（`erp/sales`）。本版答“能力不支持”（`CAPABILITY_UNSUPPORTED`，D75）的：调拨、调整、盘点、库位、期初、退货入库、分支的调整（订单修订）、序号高点与补取。

## 部署前准备

- 数据库：PostgreSQL 16 及以上；schema `erp_inventory`，属主角色 `erp_inventory_owner`（只供迁移）与运行角色 `erp_inventory_rw`（只有读写数据的权限），名字来自装配项目的注册表。三者由部署方在第一次启动前建好；表由本组件的迁移建，迁移里不建角色、不授权。
- 消息系统（`BUS_URL`）与实现实例（`IMPLEMENTATION_INSTANCE`）：仓库、余额、分支以完整状态事件、流水以增量事实发布到流 `erp.inventory`（4 个分区）。
- 部署的服务证书（`SERVICE_TLS_*`）；身份与授权（`IAM_*`、`AUTHZ_GRPC_URL`、`DEPLOYMENT_ID`）；审计（`AUDIT_GRPC_URL`）。
- 依赖 `mdm/org`（法人的状态与时区）与 `mdm/product`（产品的种类与追踪政策），都经它们的系统面读取。
- 写热点在余额行：负载高时单跑并单独分配数据库资源（协议 17）。

## 依赖说明

- 必需：数据库、消息系统、身份成员、授权成员、`mdm/org`、`mdm/product`。
- 可选：审计组件。
- `mdm/org` 或 `mdm/product` 不可达时：建仓库、入库、预占、出库答“依赖不可用”；已有数据照常读取，确认与取消照常（它们不读产品）。

## 配置指南

全部键见 `component.yaml` 的 `configSchema`（由 SDK 生成，测试核对）。

| 键 | 说明 |
| --- | --- |
| `DEPLOYMENT_ID`、`IAM_ISSUER`、`IAM_JWKS_URL`、`AUTHZ_GRPC_URL`、`SERVICE_TLS_CERT_FILE`、`SERVICE_TLS_KEY_FILE`、`SERVICE_TLS_CA_FILE`（必填） | 受保护的用户面与 mTLS 的系统面要用；部署身份、身份与授权的地址在外壳里是进程级的键 |
| `PG_HOST`、`PG_DATABASE`、`PG_USER`、`PG_PASSWORD`、`PG_SCHEMA`（必填） | 自己的数据库，密码是密钥；迁移另用 `PG_OWNER_USER`、`PG_OWNER_PASSWORD` |
| `BUS_URL`（必填，密钥）、`IMPLEMENTATION_INSTANCE`（必填） | 消息系统；实现实例是安装时生成的随机身份 |
| `AUDIT_GRPC_URL` | 审计组件的系统面；缺时审计记录留在本地，按协议 03 处理 |

`mdm/org` 与 `mdm/product` 的地址由 BrickKit 按依赖注入（`MDM_ORG_GRPC_ENDPOINT`、`MDM_PRODUCT_GRPC_ENDPOINT`），不是配置键。

## 契约索引

- **系统面**（本仓库 `contract/proto`，模块 `github.com/brickKit/erp-inventory/contract/v3`，标签 `contract/v3.…`）：
  - `be.erp.inventory.v3.ReservationService/Hold`、`Confirm`、`Cancel`、`Query`（`Adjust` 本版不支持）：只对 `erp/sales` 开放，操作 `erp.inventory.system.reserve`。
  - `be.erp.inventory.v3.StockService/Issue`：只对 `erp/sales` 开放，操作 `erp.inventory.system.issue`；`Positions`：任一组件，操作 `erp.inventory.system.read`。
- **用户面**：`contract/openapi.yaml`。操作 `erp.inventory.warehouse.read`、`erp.inventory.warehouse.manage`、`erp.inventory.stock.read`、`erp.inventory.movement.read`、`erp.inventory.movement.create`（`erp.inventory.adjustment.create` 本版不支持）；资源带法人与仓库两个维度（“库管只看一个仓库”）。
- **事件**（流 `erp.inventory`，schema 在 `contract/events/`）：`erp.warehouse.changed.v1`、`erp.stock_position.changed.v1`、`erp.inventory_branch.changed.v1`（完整状态），`erp.inventory_movement.recorded.v1`（增量事实，按 `fact_id` 去重）。

## 外壳声明

不是外壳。可以被 Go 的核心外壳（`be/go-core`）承载。

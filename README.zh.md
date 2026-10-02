[English](README.md) · [中文](README.zh.md)

# erp/inventory

库存余额、流水与预留，带防超卖；实物库存变动的唯一写入方。

## 在项目里使用

```bash
brickkit add erp/inventory@2.0.0
brickkit up
```

先看 BRICKKIT.zh.md 的"部署前准备"（PostgreSQL 的 schema 与登录角色、NATS、权限与 JWKS 地址，以及给用户分配仓库授权）。

## 文档

| 想知道 | 读 |
|---|---|
| 它做什么、不做什么，怎么配置，要准备什么 | [BRICKKIT.zh.md](BRICKKIT.zh.md) |
| 它的 gRPC、REST 与事件契约 | [contracts/](contracts/) |
| 为什么这样设计：边界、防超卖、预留、仓库范围、未决问题 | [docs/design.zh.md](docs/design.zh.md) |
| 它依赖什么（什么都不依赖）与配置键 | [component.yaml](component.yaml) |
| 怎么开发它：代码地图、测试、易错点 | [AGENTS.zh.md](AGENTS.zh.md) |

## 开发

Go 1.25、Gin、`database/sql` + pgx，迁移经 be-sdk-go 用 golang-migrate。测试要一个真实的 PostgreSQL（`TEST_PG_DSN`）；确切命令与成功的样子见 AGENTS.zh.md 的"构建与测试"。在 BrickEnterprise 项目里，项目根 `make verify ID=erp/inventory FOCUS=1` 会以容器与本机进程两种形态真机跑一遍，然后收尾。

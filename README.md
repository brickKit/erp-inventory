# erp-inventory

组件 `erp/inventory`：仓库、流水、余额、批次与序列号、预占分支。使用说明见 `BRICKKIT.md`，修改说明见 `AGENTS.md`，设计见 `docs/design.md`。

旧代码在 `legacy/v2` 分支，按终局设计从零开始（D59），版本从 `3.0.0` 起（A110）。

## 在项目里使用

在装配项目里 `brickkit add erp/inventory@<版本>`，再按 `BRICKKIT.md` 的“部署前准备”配置。种子数据：`make seed`（见 `cmd/seed`）。

## 文档

| 问题 | 看哪里 |
| --- | --- |
| 它做什么、怎样部署与配置 | `BRICKKIT.md` |
| 为什么这样设计：术语、数据、规则、未决问题 | `docs/design.md`；可用量与分支的细则 `docs/rules/availability.md` |
| 怎样改它 | `AGENTS.md` |
| 契约 | `contract/README.md` |

## 开发

见 `AGENTS.md` 的“构建与测试”。

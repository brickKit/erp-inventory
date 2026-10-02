ID      := erp/inventory
REPO    := erp-inventory
VERSION := $(shell grep -m1 -E '^  version:' component.yaml | awk '{print $$2}')
MAJOR   := $(firstword $(subst ., ,$(VERSION)))
# 项目根：本组件在项目里固定挂在 components/<scope>/<name>/ 下
ROOT    := ../../..

.DEFAULT_GOAL := help
.PHONY: help all check-version test migrate-idempotent dag-check contract-check import-scan module-check docs-check smoke image seed db-reset

help:  ## 列出所有目标
	@awk 'BEGIN{FS=":.*##"; printf "\n用法: make <目标>\n\n"} \
	     /^[a-zA-Z0-9_-]+:.*##/ {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2} \
	     /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0,5)}' $(MAKEFILE_LIST)
	@echo ""

##@ 汇总
all: check-version test migrate-idempotent dag-check contract-check import-scan module-check docs-check  ## 全部门禁（不含要起容器的 smoke / image）

##@ 门禁
check-version:  ## metadata.version、go.mod 的主版本、HEAD 上的 tag 三者一致
	@test -n "$(VERSION)" || { echo "✗ component.yaml 里读不到 metadata.version"; exit 1; }
	@mod="$$(head -1 go.mod | awk '{print $$2}')"; \
	 if [ "$(MAJOR)" -ge 2 ] && [ "$${mod##*/}" != "v$(MAJOR)" ]; then \
	   echo "✗ version $(VERSION) 的主版本是 $(MAJOR)，go.mod 的模块路径应以 /v$(MAJOR) 结尾（实际 $$mod）"; exit 1; fi
	@tags="$$(git tag --points-at HEAD | grep -E '^v?[0-9]+\.[0-9]+\.[0-9]+$$' || true)"; \
	 if [ -n "$$tags" ]; then \
	   for want in "$(VERSION)" "v$(VERSION)"; do \
	     echo "$$tags" | grep -qx "$$want" || { echo "✗ HEAD 有版本 tag（$$(echo $$tags)），但缺 $$want：Go 组件要 $(VERSION) 与 v$(VERSION) 两个 tag 在同一提交"; exit 1; }; \
	   done; \
	   extra="$$(echo "$$tags" | grep -vx -e "$(VERSION)" -e "v$(VERSION)" || true)"; \
	   [ -z "$$extra" ] || { echo "✗ HEAD 上的版本 tag $$extra 与 component.yaml 的 $(VERSION) 不一致"; exit 1; }; \
	 fi
	@echo "✓ version=$(VERSION)（go.mod 主版本一致；HEAD 上没有 tag，或 $(VERSION) 与 v$(VERSION) 都在）"

test:  ## L1–L3 测试。需要 TEST_PG_DSN（指向 brickkit_test_db）；TEST_NATS_URL 可选
	@# 连库测试在没有 TEST_PG_DSN 时 t.Skip，go test 照样打印 ok、退出 0——全部跳过却
	@# 显示通过。所以这里先拦住，大声失败。
	@test -n "$$TEST_PG_DSN" || { echo "✗ 没设 TEST_PG_DSN：连库测试会全部 SKIP 而显示 ok（设法见 AGENTS.md 的 Build and test）"; exit 1; }
	go test ./... -race -count=1

migrate-idempotent:  ## 同一份迁移连跑两次都成功。需要 PG_HOST PG_PORT PG_DATABASE PG_USER PG_PASSWORD PG_SCHEMA（以 erp_inventory_rw 登录）
	@# 例：PG_HOST=localhost PG_PORT=5432 PG_DATABASE=brickkit_test_db PG_USER=erp_inventory_rw
	@#     PG_PASSWORD=… PG_SCHEMA=erp_inventory make migrate-idempotent
	@mkdir -p build && go build -o build/migrate-probe ./backend/cmd/migrate
	@build/migrate-probe up && build/migrate-probe up && echo "✓ 迁移幂等"

dag-check:  ## 没有任何依赖：库存是物理命令枢纽，只被调用、自己不调任何人
	@n="$$(python3 -c 'import yaml; m = yaml.safe_load(open("component.yaml")); print(len((m.get("dependencies") or {}).get("components") or []))')"; \
	 [ "$$n" = 0 ] || { echo "✗ dependencies.components 应为空（实际 $$n 条）：枢纽加一条出边（尤其是 mdm/product），就从同步调用图的叶子变成了链上一环"; exit 1; }
	@echo "✓ 无依赖，无环"

contract-check:  ## proto 只增不改（buf lint + buf breaking，对比上一个发布 tag；还没有发布过就对比 main）
	@# 对比 main 抓不住已经提交到 main 上的破坏性改动（在 main 上工作时等于自己比自己），
	@# 所以对比上一个组件版本 tag（契约包的 gen/* tag 不算）。
	buf lint
	@base="$$(git describe --tags --abbrev=0 --exclude 'gen/*' 2>/dev/null)"; \
	 against="$${base:+.git#tag=$$base}"; against="$${against:-.git#branch=main}"; \
	 echo "buf breaking --against '$$against'"; buf breaking --against "$$against"

import-scan:  ## 只 import SDK 与自己；别的组件只能用它发布的 gen/ 契约包
	@# go list 失败（模块解析不了、编译错误）时必须失败：先单独取输出、查退出码，
	@# 再过滤。管道里直接接 grep 会吞掉 go list 的退出码，什么都没扫到也打印 ✓。
	@deps="$$(go list -deps ./...)" || { echo "✗ go list -deps ./... 失败，没法扫 import"; exit 1; }; \
	 bad="$$(printf '%s\n' "$$deps" | awk '/^github\.com\/brickKit\// \
	         && !/^github\.com\/brickKit\/($(REPO)\/v$(MAJOR)|be-sdk-go)(\/|$$)/ \
	         && !/^github\.com\/brickKit\/[^\/]+\/gen\//')"; \
	 if [ -n "$$bad" ]; then \
	   echo "✗ import 了别的组件的代码："; echo "$$bad"; exit 1; fi; \
	 echo "✓ 无组件间 import"

module-check:  ## 模块能被合进外壳：入口签名对、零 os.Getenv、零进程级初始化、栈合规
	@# 只扫 backend/module 与 backend/internal（会被合进外壳的部分）；backend/cmd 是
	@# 独立运行的装配代码，不在范围内。先去掉行内 // 注释再 grep，免得解释"为什么
	@# 不许 log.Fatal"的注释把自己判成违规；_test.go 不扫（测试自己开库是测试设施）。
	@grep -qE 'func New\(ctx context\.Context, rt \*besdk\.Runtime\) \(\*besdk\.Module, error\)' \
	   backend/module/module.go || { echo "✗ module.New 的签名不对"; exit 1; }
	@bad=""; \
	 for f in $$(find backend/module backend/internal -name '*.go' ! -name '*_test.go'); do \
	   hit="$$(sed 's://.*::' "$$f" | grep -nE 'os\.Getenv|os\.LookupEnv|os\.Environ')"; \
	   [ -n "$$hit" ] && bad="$$bad$$f: $$hit\n"; \
	 done; \
	 if [ -n "$$bad" ]; then \
	   echo "✗ 模块代码读了进程环境变量（进外壳后成员互相覆盖，不报错）："; printf '%b' "$$bad"; exit 1; fi
	@bad=""; \
	 for f in $$(find backend/module backend/internal -name '*.go' ! -name '*_test.go'); do \
	   hit="$$(sed 's://.*::' "$$f" | grep -nE 'log\.Fatal|os\.Exit|signal\.Notify|otel\.SetTracerProvider|promauto\.|prometheus\.MustRegister|gin\.New\(|gin\.Default\(|gin\.SetMode|sql\.Open|net\.Listen')"; \
	   [ -n "$$hit" ] && bad="$$bad$$f: $$hit\n"; \
	 done; \
	 if [ -n "$$bad" ]; then \
	   echo "✗ 模块碰了进程级的东西或自己装配："; printf '%b' "$$bad"; exit 1; fi
	@deps="$$(go list -deps ./backend/module/... ./backend/internal/...)" || { echo "✗ go list -deps 失败，没法核对依赖栈"; exit 1; }; \
	 bad="$$(printf '%s\n' "$$deps" | awk '/labstack\/echo|gofiber\/fiber|go-chi\/chi|jinzhu\/gorm|gorm\.io|lib\/pq/')"; \
	 if [ -n "$$bad" ]; then \
	   echo "✗ 用了锁定栈之外的库："; echo "$$bad"; exit 1; fi
	@echo "✓ 入口签名对、零 os.Getenv、零进程级初始化、栈合规"

docs-check:  ## 清单与文档的严格 lint（= 项目根 make docs-check ID=$(ID)）
	@cd $(ROOT) && brickkit lint --strict $(ID)

##@ 要起容器的检查（在项目里跑）
smoke:  ## 项目能为本组件生成部署文件（brickkit up --dry-run）
	@cd $(ROOT) && brickkit up --dry-run >/dev/null && echo "✓ smoke：up --dry-run 通过"

image:  ## brickkit build 本组件，并确认镜像里有 sh + wget（健康检查经 /bin/sh 调 wget）
	@cd $(ROOT) && brickkit build $(ID)
	@docker run --rm --entrypoint sh $(REPO):$(VERSION) -c 'wget --version >/dev/null && test -f /app/component.yaml' \
	  && echo "✓ 镜像 $(REPO):$(VERSION) 里有 sh + wget + component.yaml"

##@ 本地开发数据（只给本地 / 演示用，不进部署与 CI）
seed:  ## 灌示例库存（幂等，可重复跑）。要求本组件、infra/iam-casdoor、infra/authz 都已 brickkit up 起来
	@# 入库 / 调整走 REST，要真实的应用 JWT 与 warehouse_access 授权，所以先把 iam 与 authz
	@# 的种子身份灌好；它们不在项目里时这里先大声失败，不让后面的 curl 报一串看不懂的错。
	@for dep in infra/iam-casdoor infra/authz; do \
	   python3 -c 'import sys, yaml; ids = {c["id"] for c in (yaml.safe_load(open(sys.argv[1])) or {}).get("components") or []}; sys.exit(0 if sys.argv[2] in ids else 1)' $(ROOT)/brickkit.yaml $$dep \
	   || { echo "✗ make seed 需要 $$dep 在项目里（brickkit.yaml 里没有）：入库 / 调整要真实 JWT 与权限"; exit 1; }; \
	 done
	@$(MAKE) --no-print-directory -C $(ROOT)/components/infra/iam-casdoor seed
	@$(MAKE) --no-print-directory -C $(ROOT)/components/infra/authz seed
	@bash scripts/seed.sh

db-reset:  ## 整库重置本组件数据（migrate down 再 up）。需要与 migrate-idempotent 相同的 PG_*
	@# 没有 seed-clean：流水只增不改，逐行 DELETE 既做不到干净复原（BIGSERIAL 序列不回退），
	@# 又违背这条设计本身。想清空只能整库重置——它会清空本组件的全部数据，不止种子；
	@# 重置前先 brickkit down 本组件，免得连接池里缓存的语句撞上被删重建的表。
	@mkdir -p build && go build -o build/migrate-probe ./backend/cmd/migrate
	@build/migrate-probe down && build/migrate-probe up && echo "✓ 已重置到迁移后的初始状态（含 WH-EAST / WH-SOUTH 两个仓库）"

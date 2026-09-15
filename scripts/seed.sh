#!/usr/bin/env bash
# 本组件自己的种子数据（总纲 SOP-W-7）：
#
# ① 自成一体的丰富演示数据——自己造的假 product_id，两个迁移播种的既有
#    仓库（WH-EAST/WH-SOUTH），覆盖 RECEIVE/ISSUE/ADJUST_GAIN/ADJUST_LOSS
#    四种流水原因 + 一条未确认的在途预留（RESERVED，让 available_qty <
#    on_hand_qty 这个真实业务状态也有样本）。不需要 mdm-product 在场也能
#    独立跑通——本组件是物理命令枢纽，`dependencies.components` 永远是
#    空数组（component.yaml），product_id 对本组件是不透明外键。
#
# ② 额外探测：如果 mdm-product 的种子数据存在（反查它的 command_idempotency
#    表拿真实 product id，跟 mdm-product 自己的种子数据用同一批固定
#    idempotency_key——这不是建立依赖，只是 dev tooling 层面"能连就顺手
#    连"，找不到就优雅跳过，不影响①）。这一步是为了让 crm-opportunity
#    的 WON 商机触发 erp-sales 自动建单时，Reserve 能找到真实产品的库存
#    行——不然会走 TCC 补偿建异常待办（那条路径本身没问题，只是不是"打开
#    就是一条干净 CONFIRMED 订单"这个演示效果，见 docs/plans/00-总纲.md
#    SOP-W-7）。
#
# ⚠️ 实测踩坑：Receive/Adjust 虽然也在 gRPC InventoryService 里声明，但
# 它们的 service 层实现固定调 besdk.ScopeOf(ctx) 取 warehouse 维数据权限
# （§14.2.2），而 ScopeOf 要求的 Claims 只有 besdk.RequirePermission 这层
# Gin 中间件会塞进 ctx——gRPC 侧没有对应的验签拦截器，直接用 grpcurl 调
# 这两个方法必然 panic（"设计使然"，见 backend/internal/service/service.go
# 顶部注释：gRPC 侧数据权限透传是更大的独立工作，暂不在范围内）。所以
# Receive/Adjust 本脚本走 REST + 真实 Bearer token；Reserve/ConfirmIssue
# 这两个 TCC 内部 rpc 不调 ScopeOf，才能直接 grpcurl（同 erp-sales 调用
# 它们的方式）。这条坑记入 docs/dev/实测踩坑记录.md。
#
# ⚠️ Receive/Adjust 还会检查 dev.superuser 是不是真的有目标仓库的
# warehouse_access——光有 erp.inventory.receive/.adjust 权限键不够，本
# 脚本会先用同一个 token 调 warehouse-access admin 接口把两个仓库都
# 授权给自己（幂等，重复授予不报错）。
#
# 全程 claim-first 幂等（固定 idempotency_key），重复跑不会重复建流水。
#
# ⚠️ 只给本地开发/演示用，不出现在任何部署/CI 流程里。
#
# ⚠️ 没有 seed-clean.sh：本组件的流水表"只增不改"（见 AGENTS.md），
# 逐行 DELETE 撤销种子数据既做不到"干净复原"（BIGSERIAL 序列不会回退，
# 见总纲 SOP-W-7），又违背这条设计原则本身。想清空这批数据用 `make
# db-reset`（migrate down 再 up，真正的重置，不是删除）。
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="$(cd "$DIR/../../.." && pwd)"

C_GRN=$'\033[32m'; C_RED=$'\033[31m'; C_OFF=$'\033[0m'
ok()  { echo "${C_GRN}✓${C_OFF} $*"; }
die() { echo "${C_RED}✗${C_OFF} $*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"; }
need python3; need docker

# ⚠️ 真机踩到的坑（阶段四附加 Task 0.5）：brickKit 的 servedBy 合并
# 部署下，本组件/infra-iam-casdoor 都可能被收编进某个外壳，没有独立
# 容器、也没有发布到宿主机的端口（Casdoor 本身是带外容器，不受影响，
# 仍然走宿主机映射端口）——这条脚本因此不再直接从宿主机 curl，改成起
# 一个一次性"工具箱"容器加入 brickkit 自己的 docker 网络，全部 curl
# 改在里面跑；目标地址按 brickKit 自己给依赖方注入 *_ENDPOINT 时用的
# 同一条转换规则拼（componentId+version 转小写、"/"和"."全部替换成
# "-"——brickKit 源码 internal/manifest/servicename.go 的
# ServiceName()，已向 brickKit 确认这条规则不区分部署形态）。
component_version() {
  awk -v id="$1" '$0 ~ "^  - id: "id"$"{f=1;next} f&&/^    version:/{print $2;exit}' "$ROOT/brickkit.yaml"
}
service_name() { echo "$1-$(component_version "$1")" | tr '[:upper:]' '[:lower:]' | tr '/.' '--'; }

NET="${BRICKKIT_NET:-brickkit-$(basename "$ROOT")-net}"
docker network inspect "$NET" >/dev/null 2>&1 || die "docker 网络 $NET 不存在——先把本组件 brickkit up 起来（整套或只装这一个，servedBy 合并部署也可以）"

TOOLBOX="seed-toolbox-$$"
# ⚠️ 真机踩到的坑：--user 必须跟宿主机当前用户一致——COOKIE_JAR 是
# host 侧 mktemp 建出来的（属主是宿主机用户，权限 0600），curlimages/curl
# 镜像默认用镜像自带的非 root 用户跑，不加 --user 的话容器内的 curl
# 连自己的 cookie jar 都没权限读写（`-c`/`-b` 全部静默失败，Casdoor 返回
# "Please login first"，症状极难看出是权限问题不是登录逻辑问题）。
docker run -d --rm --name "$TOOLBOX" --network "$NET" \
  --user "$(id -u):$(id -g)" --add-host host.docker.internal:host-gateway -v /tmp:/tmp \
  curlimages/curl:latest sleep 3600 >/dev/null
curl() { docker exec -i "$TOOLBOX" curl "$@"; }

CASDOOR_URL="${CASDOOR_URL:-http://host.docker.internal:8000}"
IAM_URL="${IAM_URL:-http://$(service_name infra/iam-casdoor):8200}"
INV_REST="${INV_REST:-http://$(service_name erp/inventory):8086}"
SEED_USER="dev.superuser"
SEED_PASSWORD="DevSeed123!"
SEED_APP="local-dev-seed-app"
COOKIE_JAR="$(mktemp)"
trap 'rm -f "$COOKIE_JAR"; docker rm -f "$TOOLBOX" >/dev/null 2>&1' EXIT

curl -sf -o /dev/null "$INV_REST/healthz" || die "erp-inventory（$INV_REST）连不上，先 brickkit up"

psqlx() { docker exec -i be-postgres psql -U postgres -d brickkit_db -v ON_ERROR_STOP=1 "$@"; }

wh_id() { psqlx -tA -q -c "SET search_path TO erp_inventory; SELECT id FROM warehouses WHERE code = '$1';"; }
WH_EAST="$(wh_id WH-EAST)"; WH_SOUTH="$(wh_id WH-SOUTH)"
[ -n "$WH_EAST" ] && [ -n "$WH_SOUTH" ] || die "查不到 WH-EAST/WH-SOUTH——迁移播种数据（003_seed_warehouses）没跑？"

# infra-authz 的 bundle 是各组件每 ~15s 轮询一次拉进内存的——Makefile
# 链式调用刚跑完 infra-authz 的 seed 时，立刻拿 JWT 调自己的 REST 接口
# 有真实的竞态窗口，等 18 秒让 bundle 刷新到最新授权（同 crm-opportunity
# 的既有判据）。
echo "   等 18 秒，让本组件的权限 bundle 轮询到最新授权……"
sleep 18

echo "── 换一个真实 JWT，供调自己的 REST 接口用 ──"
curl -c "$COOKIE_JAR" -s -o /dev/null -X POST "$CASDOOR_URL/api/login" \
  -H "Content-Type: application/json" \
  -d '{"application":"app-built-in","organization":"built-in","username":"admin","password":"123","autoSignin":true,"type":"login"}'
APP_JSON="$(curl -b "$COOKIE_JAR" -s "$CASDOOR_URL/api/get-application?id=admin/$SEED_APP")"
# strict=False：Casdoor 的 customCss 字段被真实登录过一次后会带字面
# 换行符，见 docs/dev/实测踩坑记录.md C19。
CLIENT_ID="$(echo "$APP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin, strict=False)["data"]["clientId"])')"
CLIENT_SECRET="$(echo "$APP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin, strict=False)["data"]["clientSecret"])')"

ID_TOKEN="$(curl -s -X POST "$CASDOOR_URL/api/login/oauth/access_token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-urlencode "grant_type=password" \
  --data-urlencode "username=$SEED_USER" \
  --data-urlencode "password=$SEED_PASSWORD" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "client_secret=$CLIENT_SECRET" \
  --data-urlencode "scope=openid profile email" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id_token"])')"
[ -n "$ID_TOKEN" ] || die "拿不到 Casdoor id_token"

ACCESS_TOKEN="$(curl -s -X POST "$IAM_URL/api/iam/token" \
  -H "Content-Type: application/json" \
  -d "{\"casdoor_id_token\": \"$ID_TOKEN\"}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')"
[ -n "$ACCESS_TOKEN" ] || die "换应用 JWT 失败"
ok "已换到真实应用 JWT"

authed() { curl -s -H "Authorization: Bearer $ACCESS_TOKEN" -H "Content-Type: application/json" "$@"; }

# ── 拿 dev.superuser 的 sub（Casdoor 内部用户 id，不是用户名）：
# warehouse_access 表与 JWT 的 sub claim 都用这个值，跟 infra-authz 自己
# 的 seed.sh 解析方式一致 ──
USER_JSON="$(curl -b "$COOKIE_JAR" -s "$CASDOOR_URL/api/get-user?id=brickkit/$SEED_USER")"
SEED_SUB="$(echo "$USER_JSON" | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d["id"] if d else "")')"
[ -n "$SEED_SUB" ] || die "Casdoor 里找不到 $SEED_USER"

echo "── 给 dev.superuser 授权 WH-EAST/WH-SOUTH 两个仓库（幂等）──"
authed -X POST "$INV_REST/erp/inventory/warehouse-access/$SEED_SUB" -d "{\"warehouse_id\":\"$WH_EAST\"}" >/dev/null
authed -X POST "$INV_REST/erp/inventory/warehouse-access/$SEED_SUB" -d "{\"warehouse_id\":\"$WH_SOUTH\"}" >/dev/null
ok "warehouse_access 已就绪（dev.superuser）"

# ⚠️ 数据权限维度要有真实存在感（总纲 SOP-W-7）：只给 dev.superuser 一个
# 全权限账号看不出"warehouse 维数据权限"这个能力真的存在——额外给
# infra-authz 种的仓管测试用户（dev.warehouse.south，角色
# dev_warehouse_manager）只授权 WH-SOUTH 一个仓库，不给 WH-EAST，这样
# "华南仓管看不到华东仓库存"这个数据权限边界才有真实账号能登录体验，
# 不只是纯自动化测试里才存在。这里独立向 Casdoor 查这个用户名的 sub
# （同 infra-authz 自己 seed.sh 的既有判据：各组件各自查，不建交接
# 协议），查不到就说明 infra-authz 的种子身份还没跑，优雅跳过不中断
# 本组件自己的①②两步。
WAREHOUSE_MANAGER_SUB="$(curl -b "$COOKIE_JAR" -s "$CASDOOR_URL/api/get-user?id=brickkit/dev.warehouse.south" | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d["id"] if d else "")')"
if [ -n "$WAREHOUSE_MANAGER_SUB" ]; then
  authed -X POST "$INV_REST/erp/inventory/warehouse-access/$WAREHOUSE_MANAGER_SUB" -d "{\"warehouse_id\":\"$WH_SOUTH\"}" >/dev/null
  ok "warehouse_access 已就绪（dev.warehouse.south → 仅 WH-SOUTH，不含 WH-EAST，演示仓库维数据权限边界）"
else
  echo "  （没探测到 dev.warehouse.south——不是依赖，只是 infra-authz 的种子身份还没建，跳过）"
fi

receive() { # key product_id warehouse_id qty [batch] [serial]
  authed -X POST "$INV_REST/erp/inventory/movements/receive" \
    -d "{\"idempotency_key\":\"$1\",\"product_id\":\"$2\",\"warehouse_id\":\"$3\",\"qty\":\"$4\",\"batch_no\":\"${5:-}\",\"serial_no\":\"${6:-}\"}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["movement_id"])'
}
adjust() { # key product_id warehouse_id qty_delta reason
  authed -X POST "$INV_REST/erp/inventory/movements/adjust" \
    -d "{\"idempotency_key\":\"$1\",\"product_id\":\"$2\",\"warehouse_id\":\"$3\",\"qty_delta\":\"$4\",\"reason\":\"$5\"}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["movement_id"])'
}

# Reserve/ConfirmIssue 是组件间 TCC 协议（不进 REST），service 层不调
# ScopeOf——不需要 Bearer token，直接 grpcurl（同 erp-sales 调用它们的
# 方式，见脚本顶部注释）。目标地址同上（service_name），不依赖独立容器。
GRPC_PORT="$(awk -F'\t' '$2=="erp/inventory"{print $4}' "$ROOT/registry/ports.tsv")"
GRPCURL="docker run --rm --network $NET -v $DIR/contracts:/contracts:ro fullstorydev/grpcurl:latest"
TARGET="$(service_name erp/inventory):$GRPC_PORT"
CALL() { $GRPCURL -plaintext -import-path /contracts -proto erp/inventory/v1/inventory.proto -d "$1" "$TARGET" "erp.inventory.v1.InventoryService/$2"; }

reserve() { # key product_id warehouse_id qty order_id -> reservation_id
  # ⚠️ 实测踩坑：grpcurl 用 protojson 默认编排输出，proto 字段名
  # reservation_id 会变成驼峰 reservationId——跟 REST 层（gin.H 手写
  # snake_case）不是同一套命名，两边解析键名不能照抄。
  CALL "{\"idempotency_key\":\"$1\",\"order_id\":\"$5\",\"items\":[{\"product_id\":\"$2\",\"warehouse_id\":\"$3\",\"qty\":\"$4\"}]}" Reserve \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["reservationId"])'
}
confirmissue() { # key reservation_id [batch] [serial]
  CALL "{\"idempotency_key\":\"$1\",\"reservation_id\":\"$2\",\"batch_no\":\"${3:-}\",\"serial_no\":\"${4:-}\"}" ConfirmIssue >/dev/null
}

echo "── ① 自成一体：4 个自造假产品，两仓库，四种流水原因 + 一条在途预留 ──"
P_A="SEED-INV-PROD-A"   # 无批次/序列，纯数量
P_B="SEED-INV-PROD-B"   # 带批次号
P_C="SEED-INV-PROD-C"   # 带序列号
P_D="SEED-INV-PROD-D"   # 会被盘亏的

receive seed-inv-recv-a "$P_A" "$WH_EAST"  500 >/dev/null
receive seed-inv-recv-b "$P_B" "$WH_EAST"  300 "BATCH-2024-09" >/dev/null
# ⚠️ 实测踩坑：inventory_movements 有 CHECK(serial_no = '' OR abs(qty) = 1)
# ——序列号追踪的物品一条流水只能记一件，不能"50 件共用一个序列号"，
# 这条约束不认 mdm-product 官方声明的 tracking_type（erp-inventory 对
# product_id 是不透明的，约束是纯粹的流水表完整性规则）。
receive seed-inv-recv-c "$P_C" "$WH_SOUTH" 1   "" "SN-000123" >/dev/null
receive seed-inv-recv-d "$P_D" "$WH_SOUTH" 200 >/dev/null

adjust seed-inv-adj-gain "$P_A" "$WH_EAST"  20  "「本地测试」盘盈" >/dev/null
adjust seed-inv-adj-loss "$P_D" "$WH_SOUTH" -15 "「本地测试」盘亏" >/dev/null

RES_ISSUE="$(reserve seed-inv-reserve-issue "$P_B" "$WH_EAST" 50 seed-inv-demo-order-1)"
confirmissue seed-inv-confirm-issue "$RES_ISSUE"

# 故意留一条未确认的在途预留——不 Confirm 也不 Cancel，让 available_qty
# < on_hand_qty 这个真实业务状态也有样本可看。P_C 只有 1 件在手（序列号
# 追踪），预留量对应改成 1。
reserve seed-inv-reserve-pending "$P_C" "$WH_SOUTH" 1 seed-inv-demo-order-2 >/dev/null

ok "自成一体演示数据已就绪：$P_A/$P_B/$P_C/$P_D 分布在 WH-EAST/WH-SOUTH，含入库/出库/盘盈/盘亏/在途预留"

echo "── ② 探测 mdm-product 种子数据，找到就顺手给真实产品灌库存 ──"
schema_exists() {
  local out
  out="$(psqlx -tA -q -c "SELECT 1 FROM information_schema.schemata WHERE schema_name = '$1';" 2>/dev/null)" || true
  [ "$out" = "1" ]
}
real_product_id() { psqlx -tA -q -c "SET search_path TO mdm_product; SELECT result_id FROM command_idempotency WHERE idempotency_key = 'seed-product-$1';"; }

FOUND=0
if schema_exists mdm_product; then
  # ⚠️ 实测踩坑：这里原来只探测 1-4 号（mdm-product 早期只有 5 个种子
  # 产品时的量），mdm-product 扩到 12 个后，crm-opportunity 用 6 号往后
  # 的产品建的 WON 商机赢单转订单会因为这里没给对应产品灌库存，Reserve
  # 真实拿不到余额走 TCC 补偿建异常待办——不是 bug，是"下游可以、也
  # 应该倒逼上游丰富数据"这条判据（总纲 SOP-W-7）第一次真实触发：
  # 探测范围必须跟着 mdm-product 实际种了多少产品走，不能停留在建这个
  # 探测步骤那一刻的产品数量。11 号是最后一个 ACTIVE 的（12 号是刻意
  # 停用的样例），11 也一起灌无害（不透明外键，erp-inventory 不关心
  # 对方是不是 DISABLED）。
  for i in 1 2 3 4 5 6 7 8 9 10 11 12; do
    PID="$(real_product_id "$i")"
    if [ -n "$PID" ]; then
      # ⚠️ 实测踩坑：idempotency_key 之前固定写成 seed-inv-recv-real-$i
      # （按位置编号），mdm-product 的种子数据被 seed-clean 后用新 id
      # 重建（真实 id 会变，不是稳定值）时，claim-first 幂等会让这个
      # 固定 key 永远返回"第一次那个旧 id"的缓存结果，新 id 悄悄一件
      # 库存都拿不到——crm-opportunity 的商机会正常引用新 id，Reserve
      # 找不到对应余额行，走 TCC 补偿建异常待办，且没有任何报错指出
      # 根因。key 必须带上真实解析出来的 product id 本身，id 一变自然
      # 是全新的 key，不会撞上旧缓存（记入踩坑记录 C22）。
      receive "seed-inv-recv-real-$PID" "$PID" "$WH_EAST" 200 >/dev/null
      FOUND=$((FOUND + 1))
    fi
  done
fi
if [ "$FOUND" -gt 0 ]; then
  ok "探测到 mdm-product 种子数据，已给 $FOUND 个真实产品各灌 200 件库存（WH-EAST）"
else
  echo "  （没探测到 mdm-product 种子数据——不是依赖，只是找不到就跳过，不影响①）"
fi

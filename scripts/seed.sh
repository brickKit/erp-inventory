#!/usr/bin/env bash
# 本组件自己的种子数据（make seed 调用；只给本地开发 / 演示用，不出现在任何部署与 CI 流程里）。
#
# ① 自成一体的演示数据——自造的假 product_id，两个迁移播种的仓库（WH-EAST / WH-SOUTH），
#    覆盖 RECEIVE / ISSUE / ADJUST_GAIN / ADJUST_LOSS 四种流水原因、两条未确认的在途预留
#    （RESERVED，让 available_qty < on_hand_qty 这个真实业务状态有样本），以及三行可用量
#    低于默认阈值 LOW_STOCK_THRESHOLD=10 的余额（其中一行是在途预留把可用量压下去的），
#    让 GET /stats/summary 的低库存清单与 GET /balances/list 有东西可看。不需要 mdm-product
#    在场：本组件不依赖任何组件，product_id 对它是不透明外键。
#
# ② 顺手探测：mdm-product 的种子产品在（反查它的 command_idempotency，用它自己种子数据的
#    固定 idempotency_key）就给真实产品各灌 200 件库存，找不到就跳过——这不是依赖，只是
#    让 crm-opportunity 赢单后 erp-sales 自动建单时 Reserve 找得到真实产品的库存。
#
# ⚠️ Receive / Adjust 走 REST + 真实 Bearer token：它们按调用者的 warehouse_access 过滤，
# 这份授权只有经过 RequirePermission 验签的请求才有；经 gRPC 直连时 ctx 里没有 Claims，
# 请求被拒绝。Reserve / ConfirmIssue 是组件间 TCC 协议，不按调用者过滤，直接 grpcurl
# （同 erp-sales 调用它们的方式）。
#
# ⚠️ 光有 erp.inventory.receive / .adjust 权限键不够，还要有目标仓库的 warehouse_access：
# 脚本先用同一个 token 调 warehouse-access 管理接口把两个仓库授给 dev.superuser（幂等）。
#
# 全程先声明再执行的幂等（固定 idempotency_key），重复跑不会重复建流水。
#
# ⚠️ 没有 seed-clean：流水只增不改，逐行 DELETE 既做不到干净复原（BIGSERIAL 序列不回退），
# 又违背这条设计本身。想清空用 make db-reset（migrate down 再 up）。
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="$(cd "$DIR/../../.." && pwd)"
source "$ROOT/infra/scripts/lib/seed-net.sh"

need python3; need docker

seed_net_check
with_toolbox

INV_REST="${INV_REST:-http://$(service_name erp/inventory):8086}"
SEED_USER="dev.superuser"
check_healthz "$INV_REST/healthz" "erp-inventory"

wh_id() { psqlx -tA -q -c "SET search_path TO erp_inventory; SELECT id FROM warehouses WHERE code = '$1';"; }
WH_EAST="$(wh_id WH-EAST)"; WH_SOUTH="$(wh_id WH-SOUTH)"
[ -n "$WH_EAST" ] && [ -n "$WH_SOUTH" ] || die "查不到 WH-EAST/WH-SOUTH——迁移播种数据（003_seed_warehouses）没跑？"

wait_bundle_refresh

echo "── 换一个真实 JWT，供调自己的 REST 接口用 ──"
ACCESS_TOKEN="$(get_app_jwt "$SEED_USER")"
ok "已换到真实应用 JWT"

authed() { curl -s -H "Authorization: Bearer $ACCESS_TOKEN" -H "Content-Type: application/json" "$@"; }

# ── 拿 dev.superuser 的 sub（Casdoor 内部用户 id，不是用户名）：
# warehouse_access 表与 JWT 的 sub claim 都用这个值，跟 infra-authz 自己
# 的 seed.sh 解析方式一致 ──
SEED_SUB="$(sub_of "$SEED_USER")"
[ -n "$SEED_SUB" ] || die "Casdoor 里找不到 $SEED_USER"

echo "── 给 dev.superuser 授权 WH-EAST/WH-SOUTH 两个仓库（幂等）──"
authed -X POST "$INV_REST/erp/inventory/warehouse-access/$SEED_SUB" -d "{\"warehouse_id\":\"$WH_EAST\"}" >/dev/null
authed -X POST "$INV_REST/erp/inventory/warehouse-access/$SEED_SUB" -d "{\"warehouse_id\":\"$WH_SOUTH\"}" >/dev/null
ok "warehouse_access 已就绪（dev.superuser）"

# 数据范围要有真实账号能体验：只给 dev.superuser 一个全权限账号看不出 warehouse 维
# 数据范围真的存在——infra-authz 种的仓管测试用户 dev.warehouse.south（角色
# dev_warehouse_manager）只授权 WH-SOUTH，不给 WH-EAST，"华南仓管看不到华东仓库存"
# 就能登录体验。各组件各自向 Casdoor 查 sub，不建交接协议；查不到说明 infra-authz 的
# 种子身份还没灌，跳过，不中断本组件自己的 ①② 两步。
WAREHOUSE_MANAGER_SUB="$(sub_of dev.warehouse.south)"
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

# Reserve / ConfirmIssue 是组件间 TCC 协议（不进 REST），不按调用者过滤——不需要 Bearer
# token，直接 grpcurl（见脚本顶部注释）。目标地址用 service_name 现算，进外壳后照样可达。
GRPC_PORT="$(awk -F'\t' '$2=="erp/inventory"{print $4}' "$ROOT/registry/ports.tsv")"
GRPCURL="docker run --rm --network $NET -v $DIR/contracts:/contracts:ro fullstorydev/grpcurl:latest"
TARGET="$(service_name erp/inventory):$GRPC_PORT"
CALL() { $GRPCURL -plaintext -import-path /contracts -proto erp/inventory/v1/inventory.proto -d "$1" "$TARGET" "erp.inventory.v1.InventoryService/$2"; }

reserve() { # key product_id warehouse_id qty order_id -> reservation_id
  # ⚠️ grpcurl 用 protojson 输出，proto 字段名 reservation_id 会变成驼峰 reservationId——
  # 与 REST 层（snake_case）不是同一套命名，两边解析键名不能照抄。
  CALL "{\"idempotency_key\":\"$1\",\"order_id\":\"$5\",\"items\":[{\"product_id\":\"$2\",\"warehouse_id\":\"$3\",\"qty\":\"$4\"}]}" Reserve \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["reservationId"])'
}
confirmissue() { # key reservation_id [batch] [serial]
  CALL "{\"idempotency_key\":\"$1\",\"reservation_id\":\"$2\",\"batch_no\":\"${3:-}\",\"serial_no\":\"${4:-}\"}" ConfirmIssue >/dev/null
}

echo "── ① 自成一体：6 个自造假产品，两仓库，四种流水原因、两条在途预留、三行低库存 ──"
P_A="SEED-INV-PROD-A"   # 无批次/序列，纯数量
P_B="SEED-INV-PROD-B"   # 带批次号
P_C="SEED-INV-PROD-C"   # 带序列号（在手 1、在途预留 1 → 可用 0，低库存）
P_D="SEED-INV-PROD-D"   # 会被盘亏的
P_E="SEED-INV-PROD-E"   # 在手 6，低于默认阈值 10（低库存）
P_F="SEED-INV-PROD-F"   # 在手 30、在途预留 25 → 可用 5（在途预留压出来的低库存）

receive seed-inv-recv-a "$P_A" "$WH_EAST"  500 >/dev/null
receive seed-inv-recv-b "$P_B" "$WH_EAST"  300 "BATCH-2024-09" >/dev/null
# ⚠️ inventory_movements 有 CHECK(serial_no = '' OR abs(qty) = 1)：序列号追踪的物品一条
# 流水只能记一件，不能"50 件共用一个序列号"。这条约束不看 mdm-product 声明的
# tracking_type（product_id 对本组件是不透明的），是纯粹的流水表完整性规则。
receive seed-inv-recv-c "$P_C" "$WH_SOUTH" 1   "" "SN-000123" >/dev/null
receive seed-inv-recv-d "$P_D" "$WH_SOUTH" 200 >/dev/null
receive seed-inv-recv-e "$P_E" "$WH_EAST"  6   >/dev/null
receive seed-inv-recv-f "$P_F" "$WH_SOUTH" 30  >/dev/null

adjust seed-inv-adj-gain "$P_A" "$WH_EAST"  20  "「本地测试」盘盈" >/dev/null
adjust seed-inv-adj-loss "$P_D" "$WH_SOUTH" -15 "「本地测试」盘亏" >/dev/null

RES_ISSUE="$(reserve seed-inv-reserve-issue "$P_B" "$WH_EAST" 50 seed-inv-demo-order-1)"
confirmissue seed-inv-confirm-issue "$RES_ISSUE"

# 故意留两条未确认的在途预留——不 Confirm 也不 Cancel，让 available_qty < on_hand_qty
# 有样本可看。P_C 只有 1 件在手（序列号追踪），预留 1；P_F 预留 25，可用只剩 5。
reserve seed-inv-reserve-pending "$P_C" "$WH_SOUTH" 1 seed-inv-demo-order-2 >/dev/null
reserve seed-inv-reserve-pending-f "$P_F" "$WH_SOUTH" 25 seed-inv-demo-order-3 >/dev/null

ok "自成一体演示数据已就绪：$P_A–$P_F 分布在 WH-EAST/WH-SOUTH，含入库/出库/盘盈/盘亏/在途预留/低库存"

echo "── ② 探测 mdm-product 种子数据，找到就顺手给真实产品灌库存 ──"
schema_exists() {
  local out
  out="$(psqlx -tA -q -c "SELECT 1 FROM information_schema.schemata WHERE schema_name = '$1';" 2>/dev/null)" || true
  [ "$out" = "1" ]
}
real_product_id() { idfor mdm_product "seed-product-$1"; }

FOUND=0
if schema_exists mdm_product; then
  # 探测范围跟着 mdm-product 实际种了多少产品走（现在是 12 个）：少探测一个，用那个
  # 产品的 WON 商机赢单转订单时 Reserve 就拿不到余额、走 TCC 补偿建异常待办。12 号是
  # 刻意停用的样例，一起灌也无害（不透明外键，本组件不关心对方是不是 DISABLED）。
  for i in 1 2 3 4 5 6 7 8 9 10 11 12; do
    PID="$(real_product_id "$i")"
    if [ -n "$PID" ]; then
      # ⚠️ idempotency_key 必须带上真实的 product id，不能按位置编号：mdm-product 的种子
      # 被 seed-clean 后重建时 id 会变，按位置编号的固定 key 会让幂等永远返回第一次那个
      # 旧 id 的结果，新 id 悄悄一件库存都拿不到，之后的 Reserve 找不到余额且没有任何报错。
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

echo "── 用同一个 token 看一眼新读端点（dev.superuser 授权了两个仓库）──"
authed "$INV_REST/erp/inventory/warehouses" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("  仓库：" + "、".join(w["code"] for w in d["warehouses"]))'
authed "$INV_REST/erp/inventory/stats/summary" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("  统计：sku_count=%s total_on_hand_qty=%s low_stock_count=%s 低库存=%s" % (d["sku_count"], d["total_on_hand_qty"], d["low_stock_count"], ", ".join(i["product_id"] + "@" + i["warehouse_id"] + "=" + i["available_qty"] for i in d["low_stock"])))'

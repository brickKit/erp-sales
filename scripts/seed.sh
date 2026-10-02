#!/usr/bin/env bash
# 本组件的种子订单（make seed 调用；只给本地开发 / 演示用，不出现在任何部署与 CI 流程里）。
# 全程走真实 REST + 应用 JWT（建单、确认、发货、取消都是人的操作，不是组件间协议），
# 固定的 idempotency_key 让重复运行不会重复建单。
#
# 覆盖：
#   ① dev.superuser：3 条订单——CONFIRMED / SHIPPED（确认再发货）/ CANCELLED（直接取消）；
#   ② dev.sales.east（华东分部）：2 条订单——DRAFT（只建不确认）/ CONFIRMED，给 org 维
#      数据范围一份不经过商机赢单的样本（"华东销售看不到别的部门的订单"）。
#
# 不种 COMPLETED / CLOSED / SUSPENDED：前两个没有对应的 REST 命令；SUSPENDED 只在
# 补偿连续失败或权威额度判定超限时出现，要靠打断依赖或伪造财务事件才造得出来，
# 那是测试（backend/internal/tcc、backend/internal/consumer）的事，不是演示数据。
#
# 前提（Makefile 的 seed 目标先核对并灌好）：infra/iam-casdoor 与 infra/authz 的种子身份，
# mdm/customer 与 mdm/product 的种子客户 / 产品，erp/inventory 给这些产品在默认仓库
# （WH-EAST，DEFAULT_WAREHOUSE_ID）灌的库存——没有库存，确认与发货都会失败。
#
# 用法：make -C components/erp/sales seed
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="$(cd "$DIR/../../.." && pwd)"
source "$ROOT/infra/scripts/lib/seed-net.sh"
need python3; need docker

seed_net_check
with_toolbox

SALES_REST="${SALES_REST:-http://$(service_name erp/sales):8084}"
check_healthz "$SALES_REST/healthz" "erp-sales"

wait_bundle_refresh

SUPERUSER_TOKEN="$(get_app_jwt dev.superuser)"
SALES_TOKEN="$(get_app_jwt dev.sales.east)"
ok "已换到真实应用 JWT（两个身份）"

CUST1="$(idfor mdm_customer seed-customer-1)"
CUST2="$(idfor mdm_customer seed-customer-2)"
CUST3="$(idfor mdm_customer seed-customer-3)"
CUST6="$(idfor mdm_customer seed-customer-6)"
CUST9="$(idfor mdm_customer seed-customer-9)"
PROD1="$(idfor mdm_product seed-product-1)"
PROD2="$(idfor mdm_product seed-product-2)"
PROD3="$(idfor mdm_product seed-product-3)"
PROD6="$(idfor mdm_product seed-product-6)"
PROD9="$(idfor mdm_product seed-product-9)"
[ -n "$CUST1" ] && [ -n "$CUST6" ] || die "拿不到 mdm-customer 的种子客户 id——先确认 mdm/customer 的 make seed 跑成功了"
[ -n "$PROD1" ] && [ -n "$PROD6" ] || die "拿不到 mdm-product 的种子产品 id——先确认 mdm/product 的 make seed 跑成功了"

mkorder() { # token key cust prod qty -> order id
  curl -s -H "Authorization: Bearer $1" -H "Content-Type: application/json" -X POST "$SALES_REST/erp/sales/orders" \
    -d "{\"idempotency_key\":\"$2\",\"customer_id\":\"$3\",\"items\":[{\"product_id\":\"$4\",\"qty\":\"$5\"}]}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])'
}
# 状态命令：答 2xx 才算成功，否则带上响应体大声失败。确认要向 DEFAULT_WAREHOUSE_ID
# 预留库存、还要过信用额度，任何一步不过都答 4xx；不看状态码，订单会悄悄停在 DRAFT。
order_cmd() { # token id action body
  local out code
  out="$(curl -s -w '\n%{http_code}' -H "Authorization: Bearer $1" -H "Content-Type: application/json" \
    -X POST "$SALES_REST/erp/sales/orders/$2/$3" -d "$4")"
  code="${out##*$'\n'}"
  case "$code" in 2??) ;; *) die "订单 $2 的 $3 答 $code：${out%$'\n'*}" ;; esac
}
confirm_order() { order_cmd "$1" "$2" confirm "{\"idempotency_key\":\"$3\"}"; }   # token id key
ship_order()    { order_cmd "$1" "$2" ship "{\"idempotency_key\":\"$3\"}"; }      # token id key
cancel_order()  { order_cmd "$1" "$2" cancel "{\"idempotency_key\":\"$3\",\"reason\":\"$4\"}"; }   # token id key reason

echo "── ① dev.superuser：3 条订单，覆盖 CONFIRMED/SHIPPED/CANCELLED ──"
O1="$(mkorder "$SUPERUSER_TOKEN" seed-order-1 "$CUST1" "$PROD1" 5)"
confirm_order "$SUPERUSER_TOKEN" "$O1" seed-order-1-confirm
O2="$(mkorder "$SUPERUSER_TOKEN" seed-order-2 "$CUST2" "$PROD2" 8)"
confirm_order "$SUPERUSER_TOKEN" "$O2" seed-order-2-confirm
ship_order "$SUPERUSER_TOKEN" "$O2" seed-order-2-ship
O3="$(mkorder "$SUPERUSER_TOKEN" seed-order-3 "$CUST3" "$PROD3" 3)"
cancel_order "$SUPERUSER_TOKEN" "$O3" seed-order-3-cancel "「本地测试」客户改需求，取消重下"
ok "订单（dev.superuser）：$O1(CONFIRMED) $O2(SHIPPED) $O3(CANCELLED)"

echo "── ② dev.sales.east（华东分部）：2 条订单，覆盖 DRAFT/CONFIRMED，不经过商机赢单的 org 维数据样本 ──"
O4="$(mkorder "$SALES_TOKEN" seed-order-4 "$CUST6" "$PROD6" 20)"
O5="$(mkorder "$SALES_TOKEN" seed-order-5 "$CUST9" "$PROD9" 4)"
confirm_order "$SALES_TOKEN" "$O5" seed-order-5-confirm
ok "订单（dev.sales.east/华东）：$O4(DRAFT) $O5(CONFIRMED)"

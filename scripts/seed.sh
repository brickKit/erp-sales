#!/usr/bin/env bash
# 本组件自己的种子订单（总纲 SOP-W-7，从零设计）：此前本组件没有独立
# `make seed`——订单数据完全来自 `crm-opportunity` 的 WON 联带效果，
# 只覆盖 DRAFT（信用超限时）/CONFIRMED 两种状态。本组件其实有完整的
# REST 面（`POST /orders`/`confirm`/`cancel`/`ship`，不是组件间协议，
# 是真实人类操作），直接调用即可覆盖更多订单状态，不需要绕路。
#
# 覆盖：
#   ① dev.superuser：3 条订单——CONFIRMED / SHIPPED（confirm 再 ship）/
#      CANCELLED（直接取消，不确认）
#   ② dev.sales.east（华东分部）：2 条订单——DRAFT（只建不确认）/
#      CONFIRMED，给 org 维数据权限再添一份不经过 crm-opportunity 的
#      真实样本（"华东销售看不到别的部门订单"这条边界不该只靠赢单联带
#      产生的那一两条订单撑着）。
#
# ⚠️ COMPLETED/CLOSED/SUSPENDED 三个状态本次不种：前两个需要额外的
# 业务流程（收款核销/人工关闭）本阶段没有对应 REST 命令；SUSPENDED 只
# 在"Reserve 成功后补偿连续失败 3 次"这条真机故障注入路径下才会真的
# 出现（backend/internal/repo/suspend.go），构造它需要临时打断依赖
# 组件连通性，不是种子脚本这种"数据"层面的事——这条路径已有自动化测试
# 覆盖（Task 8 验证过），不必也不该在演示数据里造假。
#
# ⚠️ 前置条件（本脚本自己不建，靠 Makefile 的 seed 目标链式调用）：
#   - infra-iam-casdoor/infra-authz 的种子身份
#   - mdm-customer/mdm-product 各自的种子客户/产品
# 单独跑 `make -C components/erp/sales seed` 就会把这些都建好。
#
# ⚠️ 只给本地开发/演示用，不出现在任何部署/CI 流程里。全程走真实 REST
# + JWT，claim-first 幂等（固定 idempotency_key），重复跑不会重复建单。
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
[ -n "$CUST1" ] && [ -n "$CUST6" ] || die "拿不到 mdm-customer 的种子客户 id——先确认 make -C ../../mdm/customer seed 跑成功了"
[ -n "$PROD1" ] && [ -n "$PROD6" ] || die "拿不到 mdm-product 的种子产品 id——先确认 make -C ../../mdm/product seed 跑成功了"

mkorder() { # token key cust prod qty -> order id
  curl -s -H "Authorization: Bearer $1" -H "Content-Type: application/json" -X POST "$SALES_REST/erp/sales/orders" \
    -d "{\"idempotency_key\":\"$2\",\"customer_id\":\"$3\",\"items\":[{\"product_id\":\"$4\",\"qty\":\"$5\"}]}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])'
}
confirm_order() { # token id key
  curl -s -H "Authorization: Bearer $1" -H "Content-Type: application/json" -X POST "$SALES_REST/erp/sales/orders/$2/confirm" \
    -d "{\"idempotency_key\":\"$3\"}" >/dev/null
}
ship_order() { # token id key
  curl -s -H "Authorization: Bearer $1" -H "Content-Type: application/json" -X POST "$SALES_REST/erp/sales/orders/$2/ship" \
    -d "{\"idempotency_key\":\"$3\"}" >/dev/null
}
cancel_order() { # token id key reason
  curl -s -H "Authorization: Bearer $1" -H "Content-Type: application/json" -X POST "$SALES_REST/erp/sales/orders/$2/cancel" \
    -d "{\"idempotency_key\":\"$3\",\"reason\":\"$4\"}" >/dev/null
}

echo "── ① dev.superuser：3 条订单，覆盖 CONFIRMED/SHIPPED/CANCELLED ──"
O1="$(mkorder "$SUPERUSER_TOKEN" seed-order-1 "$CUST1" "$PROD1" 5)"
confirm_order "$SUPERUSER_TOKEN" "$O1" seed-order-1-confirm
O2="$(mkorder "$SUPERUSER_TOKEN" seed-order-2 "$CUST2" "$PROD2" 8)"
confirm_order "$SUPERUSER_TOKEN" "$O2" seed-order-2-confirm
ship_order "$SUPERUSER_TOKEN" "$O2" seed-order-2-ship
O3="$(mkorder "$SUPERUSER_TOKEN" seed-order-3 "$CUST3" "$PROD3" 3)"
cancel_order "$SUPERUSER_TOKEN" "$O3" seed-order-3-cancel "「本地测试」客户改需求，取消重下"
ok "订单（dev.superuser）：$O1(CONFIRMED) $O2(SHIPPED) $O3(CANCELLED)"

echo "── ② dev.sales.east（华东分部）：2 条订单，覆盖 DRAFT/CONFIRMED，独立于 crm-opportunity 的 org 维数据样本 ──"
O4="$(mkorder "$SALES_TOKEN" seed-order-4 "$CUST6" "$PROD6" 20)"
O5="$(mkorder "$SALES_TOKEN" seed-order-5 "$CUST9" "$PROD9" 4)"
confirm_order "$SALES_TOKEN" "$O5" seed-order-5-confirm
ok "订单（dev.sales.east/华东）：$O4(DRAFT) $O5(CONFIRMED)"

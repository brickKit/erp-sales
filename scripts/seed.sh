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

C_GRN=$'\033[32m'; C_RED=$'\033[31m'; C_OFF=$'\033[0m'
ok()  { echo "${C_GRN}✓${C_OFF} $*"; }
die() { echo "${C_RED}✗${C_OFF} $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"; }
need curl; need python3; need docker

CASDOOR_URL="${CASDOOR_URL:-http://localhost:8000}"
IAM_URL="${IAM_URL:-http://localhost:8200}"
SALES_REST="${SALES_REST:-http://localhost:8084}"
SEED_PASSWORD="DevSeed123!"
SEED_APP="local-dev-seed-app"
COOKIE_JAR="$(mktemp)"
trap 'rm -f "$COOKIE_JAR"' EXIT

curl -sf -o /dev/null "$SALES_REST/healthz" || die "erp-sales（$SALES_REST）连不上，先 brickkit up"

psqlx() { docker exec -i be-postgres psql -U postgres -d brickkit_db -v ON_ERROR_STOP=1 "$@"; }
idfor() { psqlx -tA -q -c "SET search_path TO $1; SELECT result_id FROM command_idempotency WHERE idempotency_key = '$2';"; }

echo "   等 18 秒，让本组件的权限 bundle 轮询到最新授权……"
sleep 18

curl -c "$COOKIE_JAR" -s -o /dev/null -X POST "$CASDOOR_URL/api/login" \
  -H "Content-Type: application/json" \
  -d '{"application":"app-built-in","organization":"built-in","username":"admin","password":"123","autoSignin":true,"type":"login"}'
APP_JSON="$(curl -b "$COOKIE_JAR" -s "$CASDOOR_URL/api/get-application?id=admin/$SEED_APP")"
CLIENT_ID="$(echo "$APP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin, strict=False)["data"]["clientId"])')"
CLIENT_SECRET="$(echo "$APP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin, strict=False)["data"]["clientSecret"])')"

get_jwt() {
  local username="$1"
  local id_token access_token
  id_token="$(curl -s -X POST "$CASDOOR_URL/api/login/oauth/access_token" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "grant_type=password" \
    --data-urlencode "username=$username" \
    --data-urlencode "password=$SEED_PASSWORD" \
    --data-urlencode "client_id=$CLIENT_ID" \
    --data-urlencode "client_secret=$CLIENT_SECRET" \
    --data-urlencode "scope=openid profile email" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id_token"])')"
  [ -n "$id_token" ] || die "拿不到 $username 的 Casdoor id_token"
  access_token="$(curl -s -X POST "$IAM_URL/api/iam/token" \
    -H "Content-Type: application/json" \
    -d "{\"casdoor_id_token\": \"$id_token\"}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')"
  [ -n "$access_token" ] || die "$username 换应用 JWT 失败"
  echo "$access_token"
}

SUPERUSER_TOKEN="$(get_jwt dev.superuser)"
SALES_TOKEN="$(get_jwt dev.sales.east)"
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

#!/usr/bin/env bash
# 撤销 seed.sh 灌的订单：按种子用的 idempotency_key 反查 command_idempotency 拿到真实
# 订单 id 再精确删除，不按名字或客户模糊匹配。
#
# 商机赢单自动转成的订单不在这里删：它们归 crm/opportunity 的 seed-clean（那边知道
# 自己种了哪几个赢单商机，按 crm-won:<opportunity_id> 反查更精确）。
set -euo pipefail
C_GRN=$'\033[32m'; C_OFF=$'\033[0m'
ok() { echo "${C_GRN}✓${C_OFF} $*"; }

psqlx() { docker exec -i be-postgres psql -U postgres -d brickkit_db -v ON_ERROR_STOP=1 "$@"; }

psqlx -q <<'SQL'
SET search_path TO erp_sales;
DO $$
DECLARE
  oid text;
  oids text[] := ARRAY[]::text[];
  k text;
BEGIN
  FOREACH k IN ARRAY ARRAY[
    'seed-order-1','seed-order-2','seed-order-3','seed-order-4','seed-order-5'
  ] LOOP
    SELECT result_id INTO oid FROM command_idempotency WHERE idempotency_key = k;
    IF oid IS NOT NULL AND oid <> '' THEN
      oids := array_append(oids, oid);
    END IF;
  END LOOP;
  IF array_length(oids, 1) > 0 THEN
    DELETE FROM sales_order_items WHERE order_id::text = ANY(oids);
    DELETE FROM sales_orders WHERE id::text = ANY(oids);
    DELETE FROM command_idempotency WHERE idempotency_key LIKE 'seed-order-%';
    RAISE NOTICE '删除订单: %', oids;
  END IF;
END $$;
SQL

ok "erp-sales 种子订单已清空（赢单自动转成的订单不受影响，归 crm/opportunity 管）"

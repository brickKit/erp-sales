#!/usr/bin/env bash
# 撤销 seed.sh 灌的数据。反查 command_idempotency 表拿真实行 id 再精确
# 删除，不靠名字/客户模糊匹配（同 mdm-customer/crm-opportunity 的既有
# 样板）。
#
# ⚠️ 不清理 crm-opportunity 的 WON 商机联带自动建的订单——那批订单归
# crm-opportunity 自己的 seed-clean.sh 管（它知道自己种了哪几个 WON
# 商机，反查 crm-won:<opportunity_id> 更精确），本脚本只清自己
# seed-order-* 这几条。
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

ok "erp-sales 种子订单已清空（crm-opportunity 联带的订单不受影响，归它自己管）"

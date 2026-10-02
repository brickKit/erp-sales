#!/usr/bin/env bash
# 撤销 seed.sh 灌的订单：按种子用的 idempotency_key 反查 command_idempotency 拿到真实
# 订单 id 再精确删除，不按名字或客户模糊匹配。
#
# 商机赢单自动转成的订单不在这里删：它们归 crm/opportunity 的 seed-clean（那边知道
# 自己种了哪几个赢单商机，按 crm-won:<opportunity_id> 反查更精确）。
set -euo pipefail
C_GRN=$'\033[32m'; C_OFF=$'\033[0m'
ok() { echo "${C_GRN}✓${C_OFF} $*"; }

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="$(cd "$DIR/../../.." && pwd)"

# 库与 schema 取自项目给本组件的配置（config/erp-sales.yaml，$var:NAME 从
# config/vars.yaml 取），不在这里写死：项目换了 PG_SCHEMA / PG_DATABASE，
# 撤销的也跟着是那一份。没配的键用 component.yaml 的默认值。
cfg() { # key default
	python3 - "$ROOT" "$1" "$2" <<'PY'
import os, sys, yaml
root, key, default = sys.argv[1:]
def load(rel):
    p = os.path.join(root, rel)
    return (yaml.safe_load(open(p)) or {}) if os.path.exists(p) else {}
v = str(load("config/erp-sales.yaml").get(key, default))
if v.startswith("$var:"):
    v = str(load("config/vars.yaml").get(v[len("$var:"):], ""))
print(v)
PY
}
SCHEMA="$(cfg PG_SCHEMA erp_sales)"
DATABASE="$(cfg PG_DATABASE brickkit_db)"
[[ "$SCHEMA" =~ ^[a-z][a-z0-9_]*$ ]] || { echo "✗ PG_SCHEMA \"$SCHEMA\" 不是合法的 schema 名" >&2; exit 1; }

psqlx() { docker exec -i be-postgres psql -U postgres -d "$DATABASE" -v ON_ERROR_STOP=1 "$@"; }

psqlx -q -v schema="$SCHEMA" <<'SQL'
SET search_path TO :"schema";
DO $$
DECLARE
  oid text;
  oids text[] := ARRAY[]::text[];
  k text;
BEGIN
  -- 迁移还没在这个库里跑过（schema 是 db-init 建的空壳）：没有可撤销的种子，什么都不做。
  IF to_regclass('command_idempotency') IS NULL THEN  -- 按上面的 search_path 解析
    RAISE NOTICE 'schema % 还没有表（迁移没跑过），没有种子订单要删', current_setting('search_path');
    RETURN;
  END IF;
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

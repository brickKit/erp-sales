-- 迁移跑完那一刻就要能写入：001 / 002 里的初始分区是写迁移那几周写死的日期
-- （2026-09 起），在之后哪一天的新库上装本组件，当周 / 当月的分区都不在，
-- 后台维护任务第一轮跑完之前的写入会报 no partition of relation。
--
-- 这里按迁移执行时的日期补齐与维护任务同一个窗口：sales_orders /
-- sales_order_items 当月起 4 个月（backend/internal/partition/monthly.go 的
-- lookAheadMonths = 3），event_outbox / event_inbox 当周起 5 周（partition.go 的
-- lookAheadWeeks = 4）。命名与维护任务一致（"表名_YYYY_MM_DD"，周分区从周一
-- 开始），先用 to_regclass 查、不存在才建，所以在已有库上是空操作，可重复执行。
--
-- 不写 schema 名和角色名：表名经迁移连接的 search_path 解析到本组件的 schema，
-- 新分区属于跑迁移的角色（PG_USER），与父表属主相同。
DO $$
DECLARE
  month_start date := date_trunc('month', (now() AT TIME ZONE 'UTC'))::date;
  week_start  date := date_trunc('week',  (now() AT TIME ZONE 'UTC'))::date;  -- ISO 周，周一
  t     text;
  from_ date;
  i     int;
BEGIN
  FOR i IN 0..3 LOOP
    from_ := (month_start + make_interval(months => i))::date;
    FOREACH t IN ARRAY ARRAY['sales_orders', 'sales_order_items'] LOOP
      IF to_regclass(format('%I', t || '_' || to_char(from_, 'YYYY_MM_DD'))) IS NULL THEN
        EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
          t || '_' || to_char(from_, 'YYYY_MM_DD'), t,
          from_, (from_ + interval '1 month')::date);
      END IF;
    END LOOP;
  END LOOP;

  FOR i IN 0..4 LOOP
    from_ := week_start + 7 * i;
    FOREACH t IN ARRAY ARRAY['event_outbox', 'event_inbox'] LOOP
      IF to_regclass(format('%I', t || '_' || to_char(from_, 'YYYY_MM_DD'))) IS NULL THEN
        EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
          t || '_' || to_char(from_, 'YYYY_MM_DD'), t,
          from_, from_ + 7);
      END IF;
    END LOOP;
  END LOOP;
END $$;

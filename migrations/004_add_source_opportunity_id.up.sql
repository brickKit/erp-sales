-- 由哪张商机赢单自动转成的订单（crm.opportunity.won.v1 的 opportunity_id）；
-- 空串表示手工建单。加在分区父表上：已有分区与维护任务以后建的分区都带这一列。
ALTER TABLE sales_orders ADD COLUMN IF NOT EXISTS source_opportunity_id TEXT NOT NULL DEFAULT '';

-- 商机详情页按它反查订单；绝大多数订单是手工建的，只给非空值建索引。
CREATE INDEX IF NOT EXISTS sales_orders_source_opportunity
  ON sales_orders (source_opportunity_id) WHERE source_opportunity_id <> '';

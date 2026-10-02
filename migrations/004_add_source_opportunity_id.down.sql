DROP INDEX IF EXISTS sales_orders_source_opportunity;
ALTER TABLE sales_orders DROP COLUMN IF EXISTS source_opportunity_id;

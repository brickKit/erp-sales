DROP TABLE IF EXISTS sales_order_reconciliation_queue;
DROP TABLE IF EXISTS customer_snapshots;
DROP TABLE IF EXISTS pricelist_items;
DROP TABLE IF EXISTS pricelists;
DROP TABLE IF EXISTS sales_order_items;   -- CASCADE 到所有月分区
DROP TABLE IF EXISTS sales_orders;        -- CASCADE 到所有月分区

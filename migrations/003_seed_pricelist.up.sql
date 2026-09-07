-- 种一个全局兜底价格表（设计计划 §9 第 7 条）：契约（Task 16）没有留
-- 价目表管理接口，同 mdm-product 的 UOM/erp-inventory 的仓库先例，用
-- 迁移播种。价格是占位值不是真实报价——目的只是让"消费事件 → 算价 →
-- 建单 → 确认 → 发货"这条链路能跑通。
INSERT INTO pricelists (name) VALUES ('默认价目表');

INSERT INTO pricelist_items (pricelist_id, applied_on, min_quantity, unit_price, discount, price_limit, tax_rate)
SELECT id, 'GLOBAL', 0, 100.00, 0, 0, 0.13
FROM pricelists WHERE name = '默认价目表';

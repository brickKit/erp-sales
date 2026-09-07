DELETE FROM pricelist_items WHERE pricelist_id IN (SELECT id FROM pricelists WHERE name = '默认价目表');
DELETE FROM pricelists WHERE name = '默认价目表';

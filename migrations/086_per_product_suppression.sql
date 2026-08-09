-- +goose Up
-- Dynamic Product Ads slice 4: per-product suppression + cross-sell.
--
-- The purchase burn list (migration 082) gains an optional SKU dimension: a
-- row with sku='' is the existing WHOLE-PERSON burn (bought anything / generic
-- conversion → stop the chase entirely); a row with a specific sku is a
-- PER-PRODUCT burn (bought SKU-A → stop featuring SKU-A in the dynamic creative,
-- but keep chasing the rest of the cart + cross-sell complements). The record
-- path (retargeting_product_views) filters out currently-burned SKUs so a stale
-- pixel re-fire can't re-add a bought product.
ALTER TABLE retargeting_suppressions ADD COLUMN sku TEXT NOT NULL DEFAULT '';
-- Repurpose the PK so a user can carry the whole-person row (sku='') AND
-- multiple per-SKU rows at once. Existing rows default to sku='' (whole-person).
ALTER TABLE retargeting_suppressions DROP CONSTRAINT retargeting_suppressions_pkey;
ALTER TABLE retargeting_suppressions ADD PRIMARY KEY (account_id, user_id, sku);

-- Cross-sell: the catalog product a purchase should rotate the chase toward
-- (e.g. bought the kibble → feature the treats). Empty = no designated
-- complement. The feed can carry it (complement_sku column).
ALTER TABLE products ADD COLUMN complement_sku TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE products DROP COLUMN complement_sku;
ALTER TABLE retargeting_suppressions DROP CONSTRAINT retargeting_suppressions_pkey;
DELETE FROM retargeting_suppressions WHERE sku <> '';
ALTER TABLE retargeting_suppressions ADD PRIMARY KEY (account_id, user_id);
ALTER TABLE retargeting_suppressions DROP COLUMN sku;

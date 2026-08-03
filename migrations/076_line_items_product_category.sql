-- +goose Up
-- A product's OWN IAB category, distinct from include_categories (which is
-- CONTENT targeting — what pages/context the line item runs on). Retail relevance
-- scores the sponsored product against the shopper's browsed categories, and that
-- signal is the product's category, not where it's allowed to advertise. Nullable;
-- the DSP falls back to include_categories when unset (backward compatible).
ALTER TABLE line_items ADD COLUMN product_category TEXT;

-- +goose Down
ALTER TABLE line_items DROP COLUMN product_category;

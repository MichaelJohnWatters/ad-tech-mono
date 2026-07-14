-- +goose Up

-- The committed-spend persistence has stored MICRO-dollars (1 USD = 1e6 µ)
-- since the money-precision change (per-impression CPM costs are sub-cent, so
-- cents truncate to zero) — but the columns were still named *_cents. The
-- Save/Load pair is symmetric so the round-trip was correct; the names were a
-- 10,000× misread waiting for the next reader. Rename to match reality. No
-- data conversion: the values already ARE micros.
ALTER TABLE campaign_committed_spend RENAME COLUMN settled_cents TO settled_micros;
ALTER TABLE campaign_committed_spend RENAME COLUMN reserved_cents TO reserved_micros;

-- +goose Down
ALTER TABLE campaign_committed_spend RENAME COLUMN settled_micros TO settled_cents;
ALTER TABLE campaign_committed_spend RENAME COLUMN reserved_micros TO reserved_cents;

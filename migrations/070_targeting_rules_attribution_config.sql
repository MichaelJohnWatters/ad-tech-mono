-- +goose Up
-- +goose StatementBegin

-- Per-line-item attribution overrides (gap G5). The global attribution.* config
-- keys (reporting schema) are the platform defaults; a campaign can override the
-- windows / require-viewability on its targeting_rules companion row (the same
-- 1:1 table that already carries bid_modifiers + frequency_caps JSONB). Empty
-- '{}' means "use the global defaults" — reporting's attributor reads this and
-- merges any set fields over the globals. Shape:
--   {"view_window_hours": 168, "click_window_hours": 720, "require_viewable": true, "model": "linear"}
ALTER TABLE targeting_rules ADD COLUMN IF NOT EXISTS attribution_config JSONB NOT NULL DEFAULT '{}';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE targeting_rules DROP COLUMN IF EXISTS attribution_config;
-- +goose StatementEnd

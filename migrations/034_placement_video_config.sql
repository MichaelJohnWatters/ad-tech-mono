-- +goose Up
-- Per-placement video settings the SSP applies when building a video bid
-- request (skippability, duration window, mimes, protocols, placement type,
-- dimensions). Empty {} → the SSP's standard pre-roll defaults, so existing
-- video placements are unchanged.
ALTER TABLE placements ADD COLUMN video_config JSONB DEFAULT '{}';

-- +goose Down
ALTER TABLE placements DROP COLUMN video_config;

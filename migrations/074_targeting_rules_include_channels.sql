-- +goose Up
-- Per-campaign channel allowlist: which channels (display/video/audio/native/
-- dooh/retail/ingame) a line item is eligible for. Empty = all channels
-- (backward compatible — existing campaigns run everywhere). The DSP filters a
-- bid when the request's channel isn't in a non-empty allowlist.
ALTER TABLE targeting_rules ADD COLUMN include_channels TEXT[] DEFAULT '{}';

-- +goose Down
ALTER TABLE targeting_rules DROP COLUMN include_channels;

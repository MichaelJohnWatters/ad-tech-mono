-- +goose Up
-- visibility distinguishes who reads a segment at bid time:
--   'public'      — SSP attaches to user.ext.segments on the outbound bid
--                   request; visible to every DSP that receives the request.
--   'dsp_private' — owned + read by a single DSP from its own store; never
--                   leaves the DSP. This is how DSPs apply private data
--                   (CRM lists, retargeting pixels, lookalikes built from
--                   advertiser conversions) without exposing it to rivals.
--
-- The SSP and DSP scope their queries on this column so the two read paths
-- stay cleanly separated even though they share a table in dev/test.

ALTER TABLE audience_segments
    ADD COLUMN visibility TEXT NOT NULL DEFAULT 'public'
    CHECK (visibility IN ('public', 'dsp_private'));

-- Lookup pattern is (user_id → segments) joined back to segments to filter
-- by visibility. Index segments.visibility so the join's filter doesn't
-- table-scan.
CREATE INDEX idx_segments_visibility ON audience_segments (visibility);

-- +goose Down
DROP INDEX IF EXISTS idx_segments_visibility;
ALTER TABLE audience_segments DROP COLUMN visibility;

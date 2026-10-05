-- +goose Up
-- Third-party VAST tag creatives (video standards epic, phase 3 — Wrapper):
-- a video/audio creative may reference an external VAST tag URL instead of a
-- hosted media asset. The DSP bids a VAST 4.2 Wrapper (AdCOM protocol 14)
-- whose VASTAdTagURI points here; the platform injects its signed trackers
-- at wrapper level and the PLAYER resolves the chain (no server-side fetch).
-- Exactly one of asset_url / vast_tag_url is expected for video creatives
-- (enforced at the gateway API; seed/legacy rows carry asset_url only).
-- Existing table — RLS policy from migration 029 already applies.
ALTER TABLE creatives ADD COLUMN vast_tag_url TEXT;

-- +goose Down
ALTER TABLE creatives DROP COLUMN vast_tag_url;

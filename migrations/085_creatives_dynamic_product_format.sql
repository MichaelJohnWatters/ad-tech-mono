-- +goose Up
-- Dynamic Product Ads slice 3: allow the dynamic_product creative format.
-- A dynamic_product creative's html_content is a Go template the ad server
-- assembles at render time from the advertiser's catalog + the user's carted
-- SKUs (with a static {{else}} fallback). Widen the creatives.format CHECK to
-- admit it (migration 006 pinned display/native/video/audio).
ALTER TABLE creatives DROP CONSTRAINT IF EXISTS creatives_format_check;
ALTER TABLE creatives ADD CONSTRAINT creatives_format_check
    CHECK (format IN ('display', 'native', 'video', 'audio', 'dynamic_product'));

-- +goose Down
ALTER TABLE creatives DROP CONSTRAINT IF EXISTS creatives_format_check;
ALTER TABLE creatives ADD CONSTRAINT creatives_format_check
    CHECK (format IN ('display', 'native', 'video', 'audio'));

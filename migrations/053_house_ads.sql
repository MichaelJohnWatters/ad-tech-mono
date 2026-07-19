-- +goose Up

-- house_ads holds the platform's OWN fallback creatives — the "house ad" the
-- publisher ad server serves on a no-bid when publisher_adserver.stub_on_nobid
-- is on, instead of a canned hardcoded stub. Staff define one or more house ads
-- per format; the ad server picks an enabled one (weighted) for the requested
-- format and renders its markup.
--
-- PLATFORM-GLOBAL, NOT tenant-scoped: these are the platform's own ads, managed
-- by staff at the gateway (support:read to list, support:update to mutate) — like
-- campaign_committed_spend / reservation_context, they carry NO account_id and
-- have NO RLS policy. There is no tenant that "owns" a house ad, so tenant
-- isolation does not apply; access control lives entirely at the gateway.
CREATE TABLE house_ads (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- display | video | native | audio. Which serving path uses this ad.
    format TEXT NOT NULL,
    name TEXT NOT NULL,
    -- HTML for display/native; inline VAST XML for video/audio. The ad server
    -- renders this verbatim for the format's no-fill response.
    markup TEXT NOT NULL,
    landing_url TEXT,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    -- Relative selection weight among enabled ads of the same format.
    weight INT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The picker reads enabled ads for one format; index the selection predicate.
CREATE INDEX idx_house_ads_format_enabled ON house_ads (format, enabled);

-- +goose Down
DROP TABLE house_ads;

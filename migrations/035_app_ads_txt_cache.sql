-- +goose Up
-- app-ads.txt is the in-app equivalent of ads.txt (IAB Tech Lab). It is served
-- from the app *developer's* website domain (discovered via the app store
-- listing), not the app bundle id, so we cache it keyed by developer domain.
-- Structure mirrors ads_txt_cache (migration 016); like that table this is
-- platform-global fraud data and intentionally carries no RLS policy.
CREATE TABLE app_ads_txt_cache (
    developer_domain TEXT PRIMARY KEY,
    entries JSONB NOT NULL,
    last_fetched TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'valid' CHECK (status IN ('valid', 'missing', 'error')),
    last_changed TIMESTAMPTZ
);

-- The developer domain an app publisher's app-ads.txt is served from. Empty for
-- web-only publishers; the crawler skips those. Nullable/defaulted so existing
-- rows and seed data need no backfill.
ALTER TABLE publishers ADD COLUMN developer_domain TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE publishers DROP COLUMN developer_domain;
DROP TABLE app_ads_txt_cache;

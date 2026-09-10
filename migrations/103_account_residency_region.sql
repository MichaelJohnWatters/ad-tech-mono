-- +goose Up
-- Data residency (PLAN Phase 11, item 111): pin each account to a data-residency
-- region. This deployment's home region is the `platform.region` config; an
-- account whose residency_region differs is not served here (mutations rejected)
-- and its out-of-region bid-time user data is not stored. Defaults to the same
-- 'us-east-1' as s3.region/platform.region so single-region deployments are
-- unaffected (no account is ever out-of-region by default).
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS residency_region TEXT NOT NULL DEFAULT 'us-east-1';

-- +goose Down
ALTER TABLE accounts DROP COLUMN IF EXISTS residency_region;

-- +goose Up
-- Phase 4 attribution — Privacy Sandbox Attribution Reporting API (ARA), a
-- REPORTING-ONLY low-resolution overlay alongside the deterministic/identity
-- attribution. See docs/attribution-phase4-ara.md.
--
-- CRITICAL: this stream is stored SEPARATELY and is NEVER joined into the exact
-- conversions table, so the exact-money / zero-slippage invariants on the real
-- stream are untouched. ARA never bills.

-- ara_sources: the registration log the browser can't give us back. When we
-- register an attribution SOURCE at impression time we mint a source_event_id;
-- recording (source_event_id → account, destination) lets the report-ingest
-- endpoint resolve which advertiser account a browser-posted report belongs to
-- (event reports carry source_event_id; aggregatable reports only carry the
-- destination). The browser POST is unauthenticated + cross-tenant, so ingest
-- reads this via the platform hatch.
CREATE TABLE ara_sources (
    source_event_id TEXT PRIMARY KEY,        -- we mint it (decimal uint64), unique platform-wide
    account_id      UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    destination     TEXT NOT NULL,           -- advertiser site (scheme + eTLD+1)
    campaign_id     TEXT,                    -- optional reporting context
    registered_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_ara_sources_dest ON ara_sources (destination, registered_at DESC);
CREATE INDEX idx_ara_sources_account ON ara_sources (account_id, registered_at DESC);

-- ara_reports: the raw noised/aggregated reports the browser POSTs back to the
-- reporting origin (the tracker). Aggregatable payloads stay ENCRYPTED in body —
-- decrypt is the aggregation-service mock boundary; we never read them.
CREATE TABLE ara_reports (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id              UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    report_type             TEXT NOT NULL CHECK (report_type IN ('event', 'aggregate')),
    attribution_destination TEXT NOT NULL,   -- advertiser site the browser attributed to
    source_event_id         TEXT,            -- event-level only (low-entropy source id)
    trigger_data            TEXT,            -- event-level only (coarse trigger value)
    report_id               TEXT,            -- browser's report id (dedup key)
    body                    JSONB NOT NULL,  -- the raw report as posted
    received_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_ara_reports_account ON ara_reports (account_id, received_at DESC);
-- Idempotent ingest: a browser may retry delivery. Dedup per account on the
-- browser-assigned report_id (partial — a shape may omit it).
CREATE UNIQUE INDEX idx_ara_reports_dedup ON ara_reports (account_id, report_id)
    WHERE report_id IS NOT NULL AND report_id <> '';

-- RLS: the owning advertiser account sees its own ARA rows; staff + the report
-- ingest resolve cross-tenant via the platform hatch. NULLIF empty-safe (mig 087);
-- USING doubles as the INSERT WITH CHECK.
ALTER TABLE ara_sources ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ara_sources
    USING (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

ALTER TABLE ara_reports ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ara_reports
    USING (
        account_id = NULLIF(current_setting('app.current_account_id', true), '')::UUID
        OR current_setting('app.platform_read', true) = 'on'
    );

-- +goose Down
DROP TABLE ara_reports;
DROP TABLE ara_sources;

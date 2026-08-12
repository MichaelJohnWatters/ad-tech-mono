-- +goose Up
-- Phase 4 attribution (ARA) security fix — quarantine aggregatable reports.
--
-- An aggregatable ARA report structurally carries NO source_event_id, so the
-- only thing tying it to an advertiser is the attribution_destination — which is
-- the advertiser's PUBLIC site (eTLD+1), nameable by anyone. Resolving an
-- unauthenticated browser POST to a tenant account on that basis is a cross-tenant
-- write: a third party could POST forged aggregatable reports naming a victim's
-- destination and have them land in the victim's ara_reports (see
-- docs/ara-review-findings.md, finding F1). And we can't verify/decrypt the
-- payloads without the aggregation service (the documented mock boundary), so
-- there is nothing to attribute on anyway.
--
-- So aggregatable reports no longer resolve to a tenant. They land here: a
-- platform-global holding table — NOT tenant-scoped (no account_id, no RLS, like
-- incidents / batch_runs), NEVER shown in any advertiser overlay. Staff can
-- inspect it; the tracker purges it by age. Event reports (bound by the
-- unguessable source_event_id) are unaffected and still land in ara_reports.
CREATE TABLE ara_aggregatable_quarantine (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    claimed_destination TEXT NOT NULL,   -- browser/attacker-asserted; UNVERIFIED, never trusted as a tenant key
    report_id           TEXT,            -- browser's report id (dedup key)
    body                JSONB NOT NULL,  -- the raw report as posted (payloads stay encrypted)
    received_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_ara_quarantine_recent ON ara_aggregatable_quarantine (received_at DESC);
-- Idempotent ingest: a browser may retry delivery. Dedup on the browser-assigned
-- report_id (partial — a shape may omit it). Platform-global, so not per-account.
CREATE UNIQUE INDEX idx_ara_quarantine_dedup ON ara_aggregatable_quarantine (report_id)
    WHERE report_id IS NOT NULL AND report_id <> '';

-- +goose Down
DROP TABLE ara_aggregatable_quarantine;

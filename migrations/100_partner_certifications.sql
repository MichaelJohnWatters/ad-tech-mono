-- +goose Up
-- Phase 11 #112 slice 4: certification runs. A partner runs the acceptance suite
-- (golden scenarios → conformance-scored) and each run is recorded. Passing a run
-- advances the partner sandbox → certified. Platform-global (staff + the owning
-- partner read it, scoped in code via the partner's account) — no RLS, like the
-- partners registry.
CREATE TABLE partner_certifications (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    partner_id UUID NOT NULL REFERENCES partners(id) ON DELETE CASCADE,
    passed     BOOLEAN NOT NULL,
    score      INT NOT NULL,
    total      INT NOT NULL,
    checks     JSONB NOT NULL,   -- the per-scenario CheckResult list
    run_by     TEXT,             -- JWT subject that ran it
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_partner_cert_partner ON partner_certifications (partner_id, created_at DESC);

-- +goose Down
DROP TABLE partner_certifications;

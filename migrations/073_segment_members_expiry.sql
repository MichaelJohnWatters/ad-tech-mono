-- +goose Up
-- Real-time retargeting members (cmd/audience-rt) get a TTL so a shopper who
-- abandons but never converts ages out of the retargeting audience instead of
-- being chased forever. NULL = no expiry (batch profile-builder members, CRM
-- uploads) — those are unchanged. Enforcement is READ-SIDE: the SSP/DSP member
-- lookups and the preloader exclude expired rows, so an expired member simply
-- stops being retargeted at expires_at (physical purge of dead rows is a
-- follow-up; the partial index below is for it).
ALTER TABLE audience_segment_members ADD COLUMN expires_at TIMESTAMPTZ;

CREATE INDEX idx_segment_members_expires ON audience_segment_members (expires_at)
    WHERE expires_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_segment_members_expires;
ALTER TABLE audience_segment_members DROP COLUMN expires_at;

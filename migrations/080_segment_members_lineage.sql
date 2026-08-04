-- +goose Up

-- Membership lineage: WHO enrolled this user and under WHAT correlation id.
-- Until now a membership row answered neither — upload memberships were only
-- recoverable by joining ClickHouse profile_signals, real-time retargeting
-- enrolments only lived transiently in the retargeting.enrolled webhook event,
-- and hourly profile-builder enrolments had no thread at all.
--
-- source: which writer inserted the row — api | dropzone (ingest processor),
--   retargeting (audience-rt), profile-builder (rule evaluation),
--   identity-expansion (cluster expansion), reconcile (profile_signals replay),
--   demo_upload. '' = pre-migration rows / direct SQL (seed).
-- origin_trace: the correlation id of whatever caused the write — self-typed
--   by format: "ing_<32hex>" (ingest job), a 32-hex request trace (the
--   enrolling site-visit / demo request), "batch_<32hex>" (conductor chain
--   run, joins batch_runs.run_id). '' when the writer has none.
--
-- Lineage is FIRST-WRITER-WINS: AddMembers is ON CONFLICT DO NOTHING and the
-- retargeting upsert only refreshes expires_at, so a re-run or repeat visit
-- never overwrites the original origin.
ALTER TABLE audience_segment_members ADD COLUMN source TEXT NOT NULL DEFAULT '';
ALTER TABLE audience_segment_members ADD COLUMN origin_trace TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE audience_segment_members DROP COLUMN source;
ALTER TABLE audience_segment_members DROP COLUMN origin_trace;

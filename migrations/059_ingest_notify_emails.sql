-- +goose Up

-- notify_emails — optional recipients emailed when an audience ingest job
-- reaches a terminal state (done/failed), ADR 0008 Feature 3. Captured at
-- enqueue: the uploader's email (resolved from team_members by the JWT UserID)
-- plus any additional_emails on the upload, or notify_emails from a drop-zone
-- manifest. Empty (the default) = no email. Best-effort — a send failure never
-- fails the ingest.
ALTER TABLE audience_ingest_jobs
    ADD COLUMN notify_emails TEXT[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE audience_ingest_jobs DROP COLUMN notify_emails;

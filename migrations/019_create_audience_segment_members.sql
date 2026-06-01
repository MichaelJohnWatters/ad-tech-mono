-- +goose Up
-- Audience segment membership: links user IDs to the segments they belong to.
-- audience_segments (mig 011) holds segment *definitions*; this table holds
-- the (segment, user) edges that the SSP queries on the hot path to attach
-- user.ext.segments to outbound bid requests.

CREATE TABLE audience_segment_members (
    segment_id  UUID NOT NULL REFERENCES audience_segments(id) ON DELETE CASCADE,
    user_id     TEXT NOT NULL,
    account_id  UUID NOT NULL REFERENCES accounts(id),
    added_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (segment_id, user_id)
);

-- Hot path: SSP looks up "what segments is this user in?" on every bid request.
CREATE INDEX idx_segment_members_user ON audience_segment_members (user_id);

ALTER TABLE audience_segment_members ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audience_segment_members
    USING (account_id = current_setting('app.current_account_id')::UUID);

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON audience_segment_members;
ALTER TABLE audience_segment_members DISABLE ROW LEVEL SECURITY;
DROP INDEX IF EXISTS idx_segment_members_user;
DROP TABLE audience_segment_members;

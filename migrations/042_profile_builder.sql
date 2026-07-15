-- +goose Up

-- Behavioural segmentation rule, evaluated by cmd/profile-builder over the
-- behaviour_signals Delta table. NULL = a plain (onboarded) segment. Shape:
--   {"event":"request|impression|click|conversion|view",
--    "category":"sports", "channel":"video", "campaign_id":"...",
--    "publisher_id":"...", "min_count":3, "window_days":30}
-- Rule-bearing segments get REPLACE-BY-SEGMENT membership semantics: each
-- builder run recomputes the full member set (users who no longer qualify
-- are pruned), so the rule is the source of truth, not the member rows.
ALTER TABLE audience_segments ADD COLUMN rule JSONB;

-- Materialized identity clusters: the person-level view of identity_graph,
-- rebuilt wholesale by each profile-builder run (union-find connected
-- components, min-confidence gated, household edges excluded — a household
-- groups people, it doesn't identify one). member_id is unique: an id
-- belongs to exactly one person. This is the PG SERVING COPY; the Delta
-- artifact in the lake is the replayable/versioned record.
--
-- Platform-global like identity_graph (no account_id / RLS): clusters span
-- tenants by design.
CREATE TABLE identity_clusters (
    person_id TEXT NOT NULL,
    member_id TEXT NOT NULL PRIMARY KEY,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_identity_clusters_person ON identity_clusters (person_id);

-- +goose Down
DROP TABLE identity_clusters;
ALTER TABLE audience_segments DROP COLUMN rule;

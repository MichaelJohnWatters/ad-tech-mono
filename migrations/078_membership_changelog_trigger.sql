-- +goose Up
-- A trigger is the transactional outbox for the append-based audience cache: it
-- appends to audience_membership_changelog in the SAME transaction as EVERY
-- membership write — retargeting enroll/suppress, upload, profile-builder
-- add/prune, TTL purge — with no per-writer code and no missed writers. It reads
-- the segment's visibility so the single cache writer applies the delta to the
-- right Redis set. ON CONFLICT DO NOTHING inserts don't fire AFTER INSERT, so a
-- re-add of an existing member correctly appends nothing.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION audience_membership_changelog_trg() RETURNS trigger AS $$
DECLARE
    vis text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT visibility INTO vis FROM audience_segments WHERE id = NEW.segment_id;
        INSERT INTO audience_membership_changelog (account_id, user_id, segment_id, visibility, op)
        VALUES (NEW.account_id, NEW.user_id, NEW.segment_id, COALESCE(vis, 'dsp_private'), 'add');
        RETURN NEW;
    ELSIF TG_OP = 'DELETE' THEN
        SELECT visibility INTO vis FROM audience_segments WHERE id = OLD.segment_id;
        INSERT INTO audience_membership_changelog (account_id, user_id, segment_id, visibility, op)
        VALUES (OLD.account_id, OLD.user_id, OLD.segment_id, COALESCE(vis, 'dsp_private'), 'remove');
        RETURN OLD;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;
-- +goose StatementEnd

CREATE TRIGGER audience_membership_changelog_ins
    AFTER INSERT ON audience_segment_members
    FOR EACH ROW EXECUTE FUNCTION audience_membership_changelog_trg();

CREATE TRIGGER audience_membership_changelog_del
    AFTER DELETE ON audience_segment_members
    FOR EACH ROW EXECUTE FUNCTION audience_membership_changelog_trg();

-- +goose Down
DROP TRIGGER IF EXISTS audience_membership_changelog_ins ON audience_segment_members;
DROP TRIGGER IF EXISTS audience_membership_changelog_del ON audience_segment_members;
DROP FUNCTION IF EXISTS audience_membership_changelog_trg();

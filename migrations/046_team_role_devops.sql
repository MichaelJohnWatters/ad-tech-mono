-- +goose Up
-- 'devops' team-member role: the staff ops-console operator (staff:devops in
-- pkg/auth). The CHECK is recreated because Postgres can't alter one in place.
ALTER TABLE team_members DROP CONSTRAINT team_members_role_check;
ALTER TABLE team_members ADD CONSTRAINT team_members_role_check
    CHECK (role IN ('owner', 'manager', 'analyst', 'finance', 'viewer', 'ad_ops', 'devops'));

-- +goose Down
ALTER TABLE team_members DROP CONSTRAINT team_members_role_check;
ALTER TABLE team_members ADD CONSTRAINT team_members_role_check
    CHECK (role IN ('owner', 'manager', 'analyst', 'finance', 'viewer', 'ad_ops'));

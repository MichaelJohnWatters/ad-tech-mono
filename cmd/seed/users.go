package main

import (
	"context"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"golang.org/x/crypto/bcrypt"
)

// DevAdminEmail / DevAdminPassword are the well-known dev login. Real
// deployments create real users (and never seed this) — it's a convenience so
// the login flow works out of the box locally. Rotate/remove in prod.
const (
	DevAdminEmail    = "admin@adtech.local"
	DevAdminPassword = "admin"
)

// SeedDevUsers upserts a dev platform-admin account + a team member with a
// bcrypt-hashed password, so `POST /v1/auth/login` authenticates out of the box
// (UI plan F4). Idempotent by deterministic IDs.
func (in *inserter) SeedDevUsers(ctx context.Context) error {
	accountID := idgen.Derive("account", "dev-admin")
	if _, err := in.db.ExecContext(ctx, `
INSERT INTO accounts (id, name, email, type, status, created_at, updated_at)
VALUES ($1, 'Platform Admin (dev)', $2, 'admin', 'active', now(), now())
ON CONFLICT (id) DO NOTHING`, accountID, DevAdminEmail); err != nil {
		return fmt.Errorf("seed dev admin account: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(DevAdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("seed dev admin: hash password: %w", err)
	}
	userID := idgen.Derive("user", "dev-admin")
	if _, err := in.db.ExecContext(ctx, `
INSERT INTO team_members (id, account_id, email, name, role, password_hash, status, created_at, updated_at)
VALUES ($1, $2, $3, 'Dev Admin', 'owner', $4, 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET password_hash = EXCLUDED.password_hash, status = 'active', updated_at = now()`,
		userID, accountID, DevAdminEmail, string(hash)); err != nil {
		return fmt.Errorf("seed dev admin user: %w", err)
	}

	in.log.Info("seeded dev admin user", "email", DevAdminEmail, "password", "(dev only)")
	return nil
}

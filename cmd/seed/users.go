package main

import (
	"context"
	"fmt"
	"strings"

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

	// Dev CUSTOMER logins, attached to the standard-profile accounts so the
	// portals are full of real data on first login (campaigns for the
	// advertiser, placements/deals for the publisher). Skipped with a warn
	// when the profile accounts aren't seeded (non-standard profiles).
	customers := []struct {
		email, name, userKey, accountKey string
	}{
		{"advertiser@adtech.local", "Dev Advertiser", "dev-advertiser", "adv-globex"},
		{"publisher@adtech.local", "Dev Publisher", "dev-publisher", "pub-daily-news"},
	}
	for _, c := range customers {
		accountID := idgen.Derive("account", c.accountKey)
		var exists bool
		if err := in.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM accounts WHERE id = $1)`, accountID).Scan(&exists); err != nil {
			return fmt.Errorf("check %s account: %w", c.accountKey, err)
		}
		if !exists {
			in.log.Warn("profile account missing, skipping dev customer login",
				"account_key", c.accountKey, "email", c.email)
			continue
		}
		if _, err := in.db.ExecContext(ctx, `
INSERT INTO team_members (id, account_id, email, name, role, password_hash, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'owner', $5, 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET password_hash = EXCLUDED.password_hash, status = 'active', updated_at = now()`,
			idgen.Derive("user", c.userKey), accountID, c.email, c.name, string(hash)); err != nil {
			return fmt.Errorf("seed %s: %w", c.email, err)
		}
		in.log.Info("seeded dev customer login", "email", c.email, "account_key", c.accountKey, "password", "(dev only)")
	}

	// Make EVERY seeded advertiser/publisher account loginable (same dev
	// password) so the stack is usable like prod — you can log straight into any
	// account, not just the two primaries. Idempotent: skips accounts that
	// already have a login (the primaries above, or a re-run) and the e2e
	// leftovers. Email is a slug of the account name (adv-acme → acme@…, "Daily
	// News" → daily-news@…).
	rows, err := in.db.QueryContext(ctx, `
SELECT a.id::text, a.name, a.type FROM accounts a
WHERE a.type IN ('advertiser', 'publisher')
  AND a.name NOT LIKE 'e2e%'
  AND NOT EXISTS (SELECT 1 FROM team_members tm WHERE tm.account_id = a.id)
ORDER BY a.type, a.name`)
	if err != nil {
		return fmt.Errorf("list accounts for logins: %w", err)
	}
	type acct struct{ id, name, typ string }
	var accts []acct
	for rows.Next() {
		var a acct
		if err := rows.Scan(&a.id, &a.name, &a.typ); err != nil {
			rows.Close()
			return fmt.Errorf("scan account: %w", err)
		}
		accts = append(accts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range accts {
		email := loginSlug(a.name) + "@adtech.local"
		if _, err := in.db.ExecContext(ctx, `
INSERT INTO team_members (id, account_id, email, name, role, password_hash, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'owner', $5, 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET password_hash = EXCLUDED.password_hash, status = 'active', updated_at = now()`,
			idgen.Derive("user", "login-"+a.id), a.id, email, a.name, string(hash)); err != nil {
			return fmt.Errorf("seed login for %s: %w", a.name, err)
		}
		in.log.Info("seeded account login", "email", email, "account", a.name, "type", a.typ, "password", "(dev only)")
	}
	if len(accts) > 0 {
		in.log.Info("all seeded accounts are now loginable", "extra_logins", len(accts), "password", DevAdminPassword)
	}
	return nil
}

// loginSlug turns an account name into an email local-part: lowercased, the
// "advertiser "/"publisher " prefix dropped, runs of non-alphanumerics
// collapsed to a single hyphen. "Advertiser adv-acme" → "adv-acme",
// "Daily News" → "daily-news".
func loginSlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.TrimPrefix(s, "advertiser ")
	s = strings.TrimPrefix(s, "publisher ")
	var b strings.Builder
	lastHyphen := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastHyphen = false
		} else if !lastHyphen {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

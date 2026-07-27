//go:build e2e

package harness

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Seeded dev logins (cmd/seed/users.go). All share the dev password.
const (
	DevPassword       = "admin"
	DevAdminEmail     = "admin@adtech.local"
	DevAdvertiserUser = "advertiser@adtech.local"
	DevPublisherUser  = "publisher@adtech.local"
)

// LoginAs authenticates against the gateway's real login flow (POST
// /v1/auth/login → bcrypt verify → JWT session cookie) and returns an
// *http.Client whose cookie jar carries the session — every request made
// with it is a real tenant-scoped browser session. Requires the dev
// jwt_signing secret + dev users to be seeded (standard profile does both).
func (h *Harness) LoginAs(t *testing.T, email, password string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Jar:       jar,
		Transport: retryTransport{}, // survive port-forward flaps under full-suite load
		// Don't follow the post-login persona redirect — the cookie is set
		// on the 303 itself and the redirect target isn't what's under test.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	form := url.Values{"email": {email}, "password": {password}}
	resp, err := client.Post(h.URLs.Gateway+"/v1/auth/login", "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("login %s: %v", email, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login %s: status %d, want 303 (is the jwt_signing secret seeded and the gateway restarted?)", email, resp.StatusCode)
	}
	gw, _ := url.Parse(h.URLs.Gateway)
	for _, c := range jar.Cookies(gw) {
		if c.Name == "adtech_session" {
			return client
		}
	}
	t.Fatalf("login %s: no session cookie in response", email)
	return nil
}

// GrantBalance credits an advertiser account's prepay balance directly —
// the harness twin of the seed's initial grant. Ledger-honest (topups row +
// double-entry pair + balance upsert, one tx) so balance == sum(ledger)
// holds in tests too. Idempotent per (account, key).
func (h *Harness) GrantBalance(t *testing.T, accountID string, amount float64, key string) {
	t.Helper()
	tx, err := h.DB.Begin()
	if err != nil {
		t.Fatalf("grant balance begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		t.Fatalf("grant balance tenant: %v", err)
	}
	var topupID string
	err = tx.QueryRow(
		`INSERT INTO topups (account_id, amount, currency, status, payment_method, idempotency_key)
		 VALUES ($1::uuid, $2, 'USD', 'succeeded', 'seed', $3)
		 ON CONFLICT (account_id, idempotency_key) DO NOTHING RETURNING id::text`,
		accountID, amount, key).Scan(&topupID)
	if err != nil {
		_ = tx.Commit()
		return // already granted under this key
	}
	if _, err := tx.Exec(
		`INSERT INTO ledger_entries (account_code, entry_type, amount, currency, reference_type, reference_id)
		 VALUES ('platform:cash', 'debit', $1, 'USD', 'topup', $2),
		        ('advertiser:' || $3 || ':balance', 'credit', $1, 'USD', 'topup', $2)`,
		amount, topupID, accountID); err != nil {
		t.Fatalf("grant balance ledger: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO advertiser_balances (account_id, balance, currency, updated_at)
		 VALUES ($1::uuid, $2, 'USD', now())
		 ON CONFLICT (account_id) DO UPDATE
		   SET balance = advertiser_balances.balance + $2, updated_at = now()`,
		accountID, amount); err != nil {
		t.Fatalf("grant balance upsert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("grant balance commit: %v", err)
	}
}

// CreateLoginUser inserts an active team member (bcrypt-hashed password)
// under accountID so tests can LoginAs a real tenant session — self-contained
// against harness Reset wiping the seeded dev users. The account row must
// already exist (BuildBasicWorld creates them); its accounts.type decides the
// session's persona.
func (h *Harness) CreateLoginUser(t *testing.T, accountID, email, password, role string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := h.DB.Exec(`
INSERT INTO team_members (id, account_id, email, name, role, password_hash, status, created_at, updated_at)
VALUES (gen_random_uuid(), $1::uuid, $2, 'e2e login user', $3, $4, 'active', now(), now())
ON CONFLICT DO NOTHING`, accountID, email, role, string(hash)); err != nil {
		t.Fatalf("create login user %s: %v", email, err)
	}
}

//go:build e2e

package harness

import (
	"database/sql"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
)

// Account is the test-side projection of an accounts row. Carries the
// derived UUID so subtests can pass it to other helpers.
type Account struct {
	ID         string // UUID text
	ExternalID string // friendly key the harness used to derive ID
	Type       string // admin, advertiser, publisher
	Email      string
}

// CreateAdmin inserts an admin account row. No API for this yet, so we
// SQL-INSERT directly. The returned ID is deterministic — same externalKey
// always derives to the same UUID, so re-running tests reuses fixtures
// safely after Reset.
//
// When an actual /v1/auth/signup endpoint lands, swap the body for an
// HTTP POST. Callers stay unchanged.
func (h *Harness) CreateAdmin(t *testing.T, externalKey string) Account {
	t.Helper()
	return h.createAccount(t, externalKey, "admin")
}

// CreatePublisher creates a publisher-type account.
func (h *Harness) CreatePublisher(t *testing.T, externalKey string) Account {
	t.Helper()
	return h.createAccount(t, externalKey, "publisher")
}

// CreateAdvertiser creates an advertiser-type account.
func (h *Harness) CreateAdvertiser(t *testing.T, externalKey string) Account {
	t.Helper()
	return h.createAccount(t, externalKey, "advertiser")
}

// CreateAgency creates an agency-type account (acts on behalf of managed
// advertiser accounts via the act-as flow).
func (h *Harness) CreateAgency(t *testing.T, externalKey string) Account {
	t.Helper()
	return h.createAccount(t, externalKey, "agency")
}

func (h *Harness) createAccount(t *testing.T, externalKey, accountType string) Account {
	t.Helper()
	id := idgen.Derive("account", externalKey)
	email := externalKey + "@e2e.local"
	name := externalKey

	// Advertiser accounts must be linked to a DSP via dsp_id since
	// migration 022 — the CampaignLoader filters by it. Use the "internal"
	// DSP as the default for tests; cross-DSP scenarios can add their own
	// helper. ensureInternalDSP guarantees the dsps row exists (Reset
	// doesn't touch it but seed-less fresh DB might not have it yet).
	var dspID string
	if accountType == "advertiser" {
		dspID = h.ensureInternalDSP(t)
	}

	const q = `
INSERT INTO accounts (id, name, email, type, currency, status, dsp_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'USD', 'active', NULLIF($5, '')::uuid, now(), now())
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, dsp_id = EXCLUDED.dsp_id, updated_at = now()`
	if _, err := h.DB.Exec(q, id, name, email, accountType, dspID); err != nil {
		t.Fatalf("createAccount(%s, %s): %v", externalKey, accountType, err)
	}
	// Prod-shaped per-advertiser conversion posture (G7): every advertiser has its
	// own hmac_conversion key, so under tracker.conversion_strict_advertiser_key a
	// conversion billed to it validates ONLY against that key. Mint the
	// deterministic dev key (matches fireAndConsume's conversion signing) so
	// harness-fired conversions settle under strict. Seed does the same for
	// seed-created advertisers; this covers accounts created post-reseed.
	if accountType == "advertiser" {
		h.ensureConversionKey(t, id)
	}
	return Account{ID: id, ExternalID: externalKey, Type: accountType, Email: email}
}

// ensureConversionKey mints the deterministic dev hmac_conversion key for an
// advertiser account (idempotent) and pokes the tracker's secrets warm cache so
// it's usable before the test fires a conversion. Value =
// adserving.DevConversionKey(accountID), the same value fireAndConsume signs
// conversion postbacks with.
func (h *Harness) ensureConversionKey(t *testing.T, accountID string) {
	t.Helper()
	res, err := h.DB.Exec(
		`INSERT INTO secrets (name, value, purpose, owner, account_id, status)
		 SELECT $1, $2, 'hmac_conversion', 'platform', $3::uuid, 'active'
		 WHERE NOT EXISTS (
		   SELECT 1 FROM secrets
		   WHERE purpose='hmac_conversion' AND account_id=$3::uuid AND status != 'revoked')`,
		"dev-conv-key-"+accountID, adserving.DevConversionKey(accountID), accountID)
	if err != nil {
		t.Fatalf("ensureConversionKey(%s): %v", accountID, err)
	}
	// Only nudge the warm cache when we actually inserted — avoids a needless
	// NATS round-trip on the common re-create path.
	if n, _ := res.RowsAffected(); n > 0 {
		h.PublishInvalidate(t, events.SubjectCacheInvalidateSecrets)
	}
}

// ensureInternalDSP looks up (and creates if missing) the "internal" DSP
// row in the dsps table. Returns its UUID. Test advertiser accounts attach
// to this DSP so the CampaignLoader's WHERE acc.dsp_id = ? filter includes
// the test campaigns when the "internal" DSP pod queries.
func (h *Harness) ensureInternalDSP(t *testing.T) string {
	t.Helper()
	id := idgen.Derive("dsp", "internal")
	const q = `
INSERT INTO dsps (id, name, display_name, profile_type, noise_pct, no_bid_rate, status, created_at, updated_at)
VALUES ($1, 'internal', 'Internal DSP', 'internal', 0, 0, 'active', now(), now())
ON CONFLICT (name) DO NOTHING`
	if _, err := h.DB.Exec(q, id); err != nil {
		t.Fatalf("ensureInternalDSP: %v", err)
	}
	return id
}

// AccountExists reports whether an account UUID is present. Used by step
// "caches populated" to assert independent DB state before checking caches.
func (h *Harness) AccountExists(t *testing.T, id string) bool {
	t.Helper()
	var n int
	if err := h.DB.QueryRow("SELECT 1 FROM accounts WHERE id = $1", id).Scan(&n); err != nil {
		if err == sql.ErrNoRows {
			return false
		}
		t.Fatalf("AccountExists: %v", err)
	}
	return n == 1
}

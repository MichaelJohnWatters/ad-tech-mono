//go:build e2e

// Advertiser topup tests — the money-touching API (docs/UI_BUILD_PLAN.md →
// API Gaps register), exercised over HTTP as a real logged-in advertiser
// session (real-auth mode: the seeded jwt_signing secret means the gateway
// validates sessions instead of the old dev bypass). The payment leg is the
// dev/fake instant-approve path; the accounting leg is real: an
// idempotency-keyed topups row, a double-entry ledger_entries pair, and the
// advertiser_balances upsert, all in one transaction.
package e2e

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestTopupRequiresAuth — with real auth on, an unauthenticated call gets a
// 401 from the session middleware (previously the dev bypass answered it).
func TestTopupRequiresAuth(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	resp, err := h.HTTP.Get(h.URLs.Gateway + "/v1/api/billing/topup")
	if err != nil {
		t.Fatalf("GET topup: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("unauthenticated GET = %d %s, want 401", resp.StatusCode, body)
	}
}

// TestTopupTenantFlow — the full tenant path over HTTP: login as an
// advertiser owner, read the balance, credit it (201), replay the same
// idempotency key (200 + duplicate, no double credit), reuse the key with a
// different amount (409), and assert the double-entry ledger pair landed.
func TestTopupTenantFlow(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "topup-flow")

	email := fmt.Sprintf("e2e-topup-%d@login.test", time.Now().UnixNano())
	h.CreateLoginUser(t, w.AdvAcc.ID, email, "pw-e2e", "owner")
	client := h.LoginAs(t, email, "pw-e2e")

	call := func(method, body string) (int, map[string]any) {
		t.Helper()
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, h.URLs.Gateway+"/v1/api/billing/topup", rdr)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s topup: %v", method, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		out := map[string]any{}
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}

	// Fresh world → zero balance, empty history.
	if code, res := call(http.MethodGet, ""); code != http.StatusOK || res["balance"].(float64) != 0 {
		t.Fatalf("initial GET = %d %v, want 200 balance 0", code, res)
	}

	// Credit 150 → 201, balance 150.
	key := fmt.Sprintf("e2e-key-%d", time.Now().UnixNano())
	code, res := call(http.MethodPost, fmt.Sprintf(`{"amount":150,"idempotency_key":%q}`, key))
	if code != http.StatusCreated || res["balance"].(float64) != 150 {
		t.Fatalf("topup = %d %v, want 201 balance 150", code, res)
	}
	topupID, _ := res["id"].(string)

	// Replay: 200 + duplicate flag, balance unchanged.
	code, res = call(http.MethodPost, fmt.Sprintf(`{"amount":150,"idempotency_key":%q}`, key))
	if code != http.StatusOK || res["duplicate"] != true {
		t.Errorf("replay = %d %v, want 200 duplicate", code, res)
	}
	if code, res := call(http.MethodGet, ""); code != http.StatusOK || res["balance"].(float64) != 150 {
		t.Errorf("balance after replay = %d %v, want 150 (no double credit)", code, res)
	}

	// Same key, different amount: caller bug → 409.
	if code, _ := call(http.MethodPost, fmt.Sprintf(`{"amount":999,"idempotency_key":%q}`, key)); code != http.StatusConflict {
		t.Errorf("key reuse = %d, want 409", code)
	}

	// The double-entry pair landed: one debit (platform:cash), one credit
	// (the advertiser balance account), both 150, linked to the topup row.
	h.WithTenant(t, w.AdvAcc.ID, func(tx *sql.Tx) {
		var debits, credits float64
		if err := tx.QueryRow(
			`SELECT COALESCE(SUM(amount) FILTER (WHERE entry_type='debit'), 0),
			        COALESCE(SUM(amount) FILTER (WHERE entry_type='credit'), 0)
			 FROM ledger_entries WHERE reference_type='topup' AND reference_id=$1`, topupID).
			Scan(&debits, &credits); err != nil {
			t.Fatalf("sum ledger pair: %v", err)
		}
		if debits != 150 || credits != 150 {
			t.Errorf("ledger pair = debit %v / credit %v, want 150/150", debits, credits)
		}
	})

	// Cross-tenant isolation at the APPLICATION layer: a publisher-account
	// session doesn't even reach the data — publisher roles don't carry
	// billing:view, so the RBAC gate rejects it outright. (Raw-SQL RLS can't
	// be asserted here: the dev Postgres role is a BYPASSRLS superuser — see
	// the skip in rls_test.go; the store's explicit account_id predicates
	// are the enforced tenancy layer.)
	otherEmail := fmt.Sprintf("e2e-topup-other-%d@login.test", time.Now().UnixNano())
	h.CreateLoginUser(t, w.PubAcc.ID, otherEmail, "pw-e2e", "owner")
	other := h.LoginAs(t, otherEmail, "pw-e2e")
	req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/billing/topup", nil)
	resp, err := other.Do(req)
	if err != nil {
		t.Fatalf("cross-tenant GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("publisher session GET topup = %d %s, want 403 (no billing:view)", resp.StatusCode, raw)
	}
}

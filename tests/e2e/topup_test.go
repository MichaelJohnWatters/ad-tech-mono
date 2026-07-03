//go:build e2e

// Advertiser topup tests — the money-touching API gap (docs/UI_BUILD_PLAN.md
// → API Gaps register). The payment leg is the dev/fake instant-approve
// path; the accounting leg is real: an idempotency-keyed topups row, a
// double-entry ledger_entries pair, and the advertiser_balances upsert,
// all in one transaction.
package e2e

import (
	"database/sql"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestTopupDevBypassGuard — in the dev-bypass session (no signing key) the
// caller's account is "dev-account", which isn't a real tenant UUID. The
// endpoint must serve an empty balance view on GET (not a uuid-cast 500)
// and refuse to credit on POST.
func TestTopupDevBypassGuard(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	resp, err := h.HTTP.Get(h.URLs.Gateway + "/v1/api/billing/topup")
	if err != nil {
		t.Fatalf("GET topup: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"topups":[]`) {
		t.Errorf("dev GET = %d %s, want 200 empty view", resp.StatusCode, body)
	}

	resp, err = h.HTTP.Post(h.URLs.Gateway+"/v1/api/billing/topup", "application/json",
		strings.NewReader(`{"amount":50,"idempotency_key":"e2e-dev-guard"}`))
	if err != nil {
		t.Fatalf("POST topup: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("dev POST = %d %s, want 400 (no billable account)", resp.StatusCode, body)
	}
}

// TestTopupLedgerSchema — exercises the topup write shape against the real
// schema through the harness's tenant-scoped SQL: idempotent insert (the
// ON CONFLICT replay returns no row), the double-entry ledger pair, the
// balance upsert, and RLS isolation of the topups table.
//
// NOTE: this duplicates the SQL of cmd/gateway's pgTopupStore rather than
// driving it over HTTP — the dev-bypass session has no tenant identity, so
// the HTTP tenant path needs a real-auth harness (jwt_signing secret +
// gateway.require_auth). When that lands, replace this with a POST as a
// logged-in advertiser and assert the same rows.
func TestTopupLedgerSchema(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "topup-ledger")
	acc := w.AdvAcc.ID
	key := "e2e-topup-" + w.AdvAcc.ExternalID

	// First write: topup row + ledger pair + balance upsert in one tx.
	var id string
	var balance float64
	h.WithTenant(t, acc, func(tx *sql.Tx) {
		if err := tx.QueryRow(
			`INSERT INTO topups (account_id, amount, currency, status, payment_method, idempotency_key)
			 VALUES ($1::uuid, 125, 'USD', 'succeeded', 'dev', $2)
			 ON CONFLICT (account_id, idempotency_key) DO NOTHING RETURNING id::text`,
			acc, key).Scan(&id); err != nil {
			t.Fatalf("insert topup: %v", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO ledger_entries (account_code, entry_type, amount, currency, reference_type, reference_id)
			 VALUES ('platform:cash','debit',125,'USD','topup',$1),
			        ('advertiser:' || $2 || ':balance','credit',125,'USD','topup',$1)`,
			id, acc); err != nil {
			t.Fatalf("insert ledger pair: %v", err)
		}
		if err := tx.QueryRow(
			`INSERT INTO advertiser_balances (account_id, balance, currency, updated_at)
			 VALUES ($1::uuid, 125, 'USD', now())
			 ON CONFLICT (account_id) DO UPDATE
			   SET balance = advertiser_balances.balance + EXCLUDED.balance, updated_at = now()
			 RETURNING balance`, acc).Scan(&balance); err != nil {
			t.Fatalf("upsert balance: %v", err)
		}
	})
	if balance < 125 {
		t.Errorf("balance after topup = %v, want >= 125", balance)
	}

	// Replay with the same idempotency key: the insert must return no row.
	h.WithTenant(t, acc, func(tx *sql.Tx) {
		var replayID string
		err := tx.QueryRow(
			`INSERT INTO topups (account_id, amount, currency, status, payment_method, idempotency_key)
			 VALUES ($1::uuid, 125, 'USD', 'succeeded', 'dev', $2)
			 ON CONFLICT (account_id, idempotency_key) DO NOTHING RETURNING id::text`,
			acc, key).Scan(&replayID)
		if err != sql.ErrNoRows {
			t.Errorf("replay insert: err=%v id=%q, want ErrNoRows (idempotent no-op)", err, replayID)
		}
	})

	// The ledger pair balances: one debit, one credit, same amount.
	h.WithTenant(t, acc, func(tx *sql.Tx) {
		var debits, credits float64
		if err := tx.QueryRow(
			`SELECT COALESCE(SUM(amount) FILTER (WHERE entry_type='debit'), 0),
			        COALESCE(SUM(amount) FILTER (WHERE entry_type='credit'), 0)
			 FROM ledger_entries WHERE reference_type='topup' AND reference_id=$1`, id).
			Scan(&debits, &credits); err != nil {
			t.Fatalf("sum ledger pair: %v", err)
		}
		if debits != 125 || credits != 125 {
			t.Errorf("ledger pair = debit %v / credit %v, want 125/125", debits, credits)
		}
	})

	// RLS: another tenant cannot see this topup row.
	h.WithTenant(t, w.PubAcc.ID, func(tx *sql.Tx) {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM topups WHERE idempotency_key=$1`, key).Scan(&n); err != nil {
			t.Fatalf("cross-tenant count: %v", err)
		}
		if n != 0 {
			t.Errorf("cross-tenant read sees %d topup rows, want 0 (RLS)", n)
		}
	})
}

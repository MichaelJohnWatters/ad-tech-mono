//go:build e2e

// Customer support & dispute resolution (PLAN Phase 11, item 108). A customer
// opens a billing dispute; staff see it in the cross-tenant queue, reply on the
// thread, and resolve it with a credit adjustment; the customer sees the
// resolution and can reopen by replying. Tenant isolation holds throughout.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// supDo issues an authed JSON request and returns (status, decoded-map).
func supDo(t *testing.T, client *http.Client, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, url, rdr)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func supList(t *testing.T, client *http.Client, url string) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, string(raw))
	}
	var out []map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestSupportDisputeWorkflow(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "support")
	adv := w.AdvAcc
	cust := h.OwnerClient(t, adv.ID) // advertiser owner → support:contact
	base := h.URLs.Gateway + routes.APISupportTickets

	// Customer opens a billing dispute with an opening message.
	st, created := supDo(t, cust, http.MethodPost, base, map[string]any{
		"kind": "billing_dispute", "subject": "Impression discrepancy",
		"body": "Your tracker shows 80K, my DV shows 50K.", "amount_disputed": 12.50,
	})
	if st != 200 {
		t.Fatalf("create ticket: %d %v", st, created)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("no ticket id: %v", created)
	}

	// It shows in the customer's own list.
	if mine := supList(t, cust, base); len(mine) != 1 || mine[0]["subject"] != "Impression discrepancy" {
		t.Fatalf("customer list = %v, want the one ticket", mine)
	}

	// Staff sees it in the cross-tenant queue with the account name.
	staff := h.CreateStaff(t, fmt.Sprintf("support-staff-%d", time.Now().UnixNano()))
	staffCl := h.OwnerClient(t, staff.ID) // staff owner → support:read/update
	queue := supList(t, staffCl, base+"?scope=all")
	var found map[string]any
	for _, tk := range queue {
		if tk["id"] == id {
			found = tk
		}
	}
	if found == nil {
		t.Fatalf("ticket not in staff queue")
	}
	if found["account_name"] == "" || found["account_name"] == nil {
		t.Errorf("staff queue row missing account_name")
	}

	// Staff replies, then resolves with a $5 credit.
	if st, _ := supDo(t, staffCl, http.MethodPost, base+"/"+id+"/messages", map[string]any{"body": "Investigating — viewability vs rendered count."}); st != 200 {
		t.Fatalf("staff reply: %d", st)
	}
	st, res := supDo(t, staffCl, http.MethodPost, base+"/"+id+"/resolve", map[string]any{
		"status": "resolved", "resolution": "Partial discrepancy — $5 credit issued.", "credit_amount": 5.00,
	})
	if st != 200 {
		t.Fatalf("resolve: %d %v", st, res)
	}
	if res["credited"] != true {
		t.Errorf("resolve credited = %v, want true", res["credited"])
	}

	// The credit adjustment landed on the account (canonical adjustments table).
	var adjCount int
	var adjTotal float64
	if err := h.DB.QueryRow(
		`SELECT count(*), COALESCE(SUM(amount),0) FROM adjustments WHERE account_id = $1::uuid AND type = 'credit'`,
		adv.ID).Scan(&adjCount, &adjTotal); err != nil {
		t.Fatalf("adjustments query: %v", err)
	}
	if adjCount != 1 || adjTotal < 4.999 || adjTotal > 5.001 {
		t.Errorf("credit adjustment = count %d / total %.2f, want 1 / 5.00", adjCount, adjTotal)
	}

	// Customer sees the resolution + the staff message, status resolved.
	_, detail := supDo(t, cust, http.MethodGet, base+"/"+id, nil)
	if detail["status"] != "resolved" {
		t.Errorf("customer sees status %v, want resolved", detail["status"])
	}
	if detail["resolution"] == "" || detail["resolution"] == nil {
		t.Errorf("customer sees no resolution note")
	}
	msgs, _ := detail["messages"].([]any)
	if len(msgs) < 2 {
		t.Errorf("thread has %d messages, want >=2 (opening + staff reply)", len(msgs))
	}
	// Staff identity must NOT leak to the customer (mig 094 guarantee): staff
	// messages carry no author_id, and the ticket's assignee is hidden.
	if av, ok := detail["assigned_to"]; ok && av != "" && av != nil {
		t.Errorf("customer sees assigned_to %v — staff identity leak", av)
	}
	for _, mi := range msgs {
		m, _ := mi.(map[string]any)
		if m["author_type"] == "staff" {
			if aid, ok := m["author_id"]; ok && aid != "" && aid != nil {
				t.Errorf("customer sees staff message author_id %v — staff identity leak", aid)
			}
		}
	}

	// Customer reopens by replying → status returns to open.
	if st, _ := supDo(t, cust, http.MethodPost, base+"/"+id+"/messages", map[string]any{"body": "Thanks, but please double-check the geo filter too."}); st != 200 {
		t.Fatalf("customer reply: %d", st)
	}
	_, reopened := supDo(t, cust, http.MethodGet, base+"/"+id, nil)
	if reopened["status"] != "open" {
		t.Errorf("after customer reply status = %v, want open (reopened)", reopened["status"])
	}

	// Tenant isolation: a different advertiser cannot see this ticket.
	other := h.CreateAdvertiser(t, fmt.Sprintf("support-other-%d", time.Now().UnixNano()))
	otherCl := h.OwnerClient(t, other.ID)
	if st, _ := supDo(t, otherCl, http.MethodGet, base+"/"+id, nil); st != 404 {
		t.Errorf("cross-tenant ticket read = %d, want 404", st)
	}
}

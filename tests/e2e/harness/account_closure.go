//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// AccountClosure is the test-side view of a closure request.
type AccountClosure struct {
	ID                    string    `json:"id"`
	AccountID             string    `json:"account_id"`
	Status                string    `json:"status"`
	GraceEndsAt           time.Time `json:"grace_ends_at"`
	PausedLineItems       []string  `json:"paused_line_items"`
	DeactivatedPlacements []string  `json:"deactivated_placements"`
}

type closureStatusEnvelope struct {
	Closure *AccountClosure `json:"closure"`
}

// OwnerClient logs in a fresh owner user for the account and returns an authed
// client (cookie-jar session). Reusable across account-scoped owner actions.
func (h *Harness) OwnerClient(t *testing.T, accountID string) *http.Client {
	t.Helper()
	email := "owner-" + accountID + "@e2e.local"
	h.CreateLoginUser(t, accountID, email, "e2e-pass", "owner")
	return h.LoginAs(t, email, "e2e-pass")
}

// AccountCloseStatus GETs the caller's active closure (nil when none).
func (h *Harness) AccountCloseStatus(t *testing.T, client *http.Client) *AccountClosure {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.URLs.Gateway+routes.APIAccountClose, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("account close status: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("account close status %d: %s", resp.StatusCode, string(b))
	}
	var env closureStatusEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("decode close status: %v (body=%s)", err, string(b))
	}
	return env.Closure
}

// AccountClose POSTs to initiate a closure. Returns (status, closure).
func (h *Harness) AccountClose(t *testing.T, client *http.Client) (int, AccountClosure) {
	t.Helper()
	return h.accountClosePost(t, client, routes.APIAccountClose)
}

// AccountCloseCancel POSTs to cancel a closure. Returns (status, closure).
func (h *Harness) AccountCloseCancel(t *testing.T, client *http.Client) (int, AccountClosure) {
	t.Helper()
	return h.accountClosePost(t, client, routes.APIAccountCloseCancel)
}

func (h *Harness) accountClosePost(t *testing.T, client *http.Client, url string) (int, AccountClosure) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Gateway+url, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("account close post %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var cr AccountClosure
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(b, &cr); err != nil {
			t.Fatalf("decode closure: %v (body=%s)", err, string(b))
		}
	}
	return resp.StatusCode, cr
}

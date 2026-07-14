//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// ForceSpendSnapshot POSTs to the reporting service's spend-snapshot debug
// endpoint, forcing an immediate publish of the billing engine's committed
// spend on adtech.billing.campaign_spend_snapshot (so DSP pacing reconciliation
// fires without waiting for the periodic ticker). Returns the per-campaign
// committed spend in MICRO-dollars (1 USD = 1_000_000 µ — the unit
// Engine.SnapshotCommitted keeps, because a per-impression CPM cost is
// sub-cent) as billing currently sees it.
func (h *Harness) ForceSpendSnapshot(t *testing.T) map[string]int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Reporting+routes.DebugSpendSnapshot, nil)
	if err != nil {
		t.Fatalf("force spend snapshot: %v", err)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("force spend snapshot: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("force spend snapshot status %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Committed map[string]int64 `json:"committed"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode spend snapshot: %v (%s)", err, string(body))
	}
	return out.Committed
}

// Micros converts a dollar amount to integer micro-dollars — the unit every
// committed-spend surface uses. Mirror of pkg/billing's toMicros.
func Micros(dollars float64) int64 { return int64(math.Round(dollars * 1_000_000)) }

// CommittedSpendMicros returns the billing engine's committed spend (settled +
// open reserves) for one campaign, in micro-dollars. Forces a snapshot so the
// value is current.
func (h *Harness) CommittedSpendMicros(t *testing.T, campaignID string) int64 {
	t.Helper()
	return h.ForceSpendSnapshot(t)[campaignID]
}

// WaitCommittedMicros polls the committed spend for a campaign until it equals
// want micro-dollars (billing consumers run async off NATS), failing after a
// short timeout.
func (h *Harness) WaitCommittedMicros(t *testing.T, campaignID string, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last int64
	for time.Now().Before(deadline) {
		last = h.CommittedSpendMicros(t, campaignID)
		if last == want {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("committed spend for %s = %d micro-dollars, want %d", campaignID, last, want)
}

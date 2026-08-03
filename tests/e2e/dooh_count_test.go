//go:build e2e

// DOOH count metric: the reporting impression COUNT now reads SUM(impression_qty),
// so a DOOH proof-of-play (one row, impression_qty = venue audience) reports as its
// audience, not 1 — consistent with the exact cost. Proves it through the reporting
// query engine (the raw path; the app-side rollup uses the same store.Query).
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestDOOHCountIsAudienceNotRows(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "dooh-count")

	const mult = 5 // venue audience per play
	auc := h.RunAuction(t, w.Placement.ExternalID, "GBR", "mobile", "")
	win := h.ExtractWinner(t, auc)
	if win.NoBid {
		t.Fatal("expected a winner to fire a DOOH play")
	}
	// One DOOH play = one impression row carrying impression_qty=5.
	h.FireDOOHPlay(t, auc.TraceID, win.CampaignID, win.CreativeID, auc.PlacementID, auc.PublisherID, w.AdvAcc.ID, "USD", win.Price, mult)

	// Fresh reset stack → this play is the only impression, so the platform-wide
	// dooh channel count (via the reporting query engine) must read the AUDIENCE.
	staff := h.CreateStaff(t, fmt.Sprintf("dooh-cnt-acct-%d", time.Now().UnixNano()))
	email := fmt.Sprintf("dooh-cnt-%d@e2e.local", time.Now().UnixNano())
	h.CreateLoginUser(t, staff.ID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")

	doohCount := func() int {
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+routes.APIStaffChannels, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("staff channels: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("staff channels status %d: %s", resp.StatusCode, b)
		}
		var res struct {
			Columns []string        `json:"columns"`
			Rows    [][]interface{} `json:"rows"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		ci, ni := indexOf(res.Columns, "channel"), indexOf(res.Columns, "count")
		for _, r := range res.Rows {
			if fmt.Sprint(r[ci]) == "dooh" {
				n, _ := r[ni].(float64)
				return int(n)
			}
		}
		return 0
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		n := doohCount()
		if n == mult {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dooh count = %d, want %d (SUM(impression_qty), not row count)", n, mult)
		}
		time.Sleep(2 * time.Second)
	}
}

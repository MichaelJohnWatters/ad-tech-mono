//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestBidShadingSavingsDurableAndGlobal locks the event→ClickHouse bid-shading
// savings path end-to-end:
//
//   - a genuinely SHADED win emits AuctionShadeEvent off the DSP win-notice path
//     → reporting → the auction_shades ClickHouse table;
//   - the advertiser portal (/v1/api/shading) reads the TOTAL back as
//     SUM(savings_usd) from ClickHouse — a figure aggregated across every DSP pod
//     and durable across a DSP restart (NOT a per-pod in-memory counter), and it
//     is STABLE across repeated reads (a regression guard for the hot/cold-routing
//     and per-fetch-timeout fixes that made it flap between 0 and the real value);
//   - the NEGATIVE: a campaign with shading DISABLED emits nothing, no matter how
//     many auctions it wins (shading is strictly opt-in).
//
// Shading only fires when the POOLED per-placement clearing average sits below a
// campaign's valuation — a lone first-price winner records its own price as the
// clearing, so its pooled average equals its bid and it never shades. We recreate
// the real condition (a high-bid campaign + a cheap co-bidder on the SAME
// placement, which drags the pooled average down) so the high campaign shades.
func TestBidShadingSavingsDurableAndGlobal(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "shadesave")
	adv := w.AdvAcc
	placement := w.Placement.ExternalID

	// --- NEGATIVE: shading disabled → zero savings records, however many wins. ---
	h.SetLineItemShading(t, w.Campaign.ID, adv.ID, "disabled")
	h.RefreshAllCaches(t)
	for i := 0; i < 40; i++ {
		h.RunAuction(t, placement, "GBR", "mobile", fmt.Sprintf("neg-%d", i))
	}
	time.Sleep(3 * time.Second) // win nurl + NATS drain
	if n := shadeRowCount(t, h, adv.ID); n != 0 {
		t.Fatalf("shading disabled but produced %d auction_shades rows, want 0 (shading must be opt-in)", n)
	}

	// --- POSITIVE: high-bid AGGRESSIVE campaign + cheap co-bidder on the SAME
	// placement. The cheap campaign (desktop) seeds sub-valuation clearings into
	// the pooled curve (even its losses record the modest winning price), so the
	// high campaign (mobile, bid 9.0) finds room to shade down from 9.0. ---
	cheapIO := h.CreateInsertionOrder(t, adv, "e2e-io-cheap-shadesave", 5000)
	cheap := h.CreateCampaign(t, adv, cheapIO, "e2e-li-cheap-shadesave",
		2.00, 500, "e2e-cr-cheap-shadesave", "cheap-shadesave.test",
		harness.Targeting{Geos: []string{"GBR"}, Devices: []string{"desktop"}})
	h.SetLineItemShading(t, cheap.ID, adv.ID, "disabled") // seeds clearings; never shades itself

	h.SetLineItemBaseBid(t, w.Campaign.ID, adv.ID, 9.00)
	h.SetLineItemShading(t, w.Campaign.ID, adv.ID, "aggressive")
	h.RefreshAllCaches(t)

	// Build the pooled per-placement curve: desktop auctions seed low clearings,
	// mobile auctions seed the high valuation. Interleave so the curve spans the
	// range before the shading reads it.
	for i := 0; i < 60; i++ {
		h.RunAuction(t, placement, "GBR", "desktop", fmt.Sprintf("warm-d-%d", i))
		h.RunAuction(t, placement, "GBR", "mobile", fmt.Sprintf("warm-m-%d", i))
	}
	// Now the curve spans [~2 .. 9]; further high-campaign (mobile) bids shade down.
	for i := 0; i < 150; i++ {
		h.RunAuction(t, placement, "GBR", "mobile", fmt.Sprintf("shade-%d", i))
	}

	// Poll ClickHouse for the durable savings rows, then let the async drain SETTLE
	// (win nurl + NATS are async; the second-price curve now shades a lot, so events
	// keep landing). Wait until the row count stops growing across two reads, so the
	// direct-CH read and the gateway read below see the same data.
	var rows, prev int
	deadline := time.Now().Add(70 * time.Second)
	for time.Now().Before(deadline) {
		rows = shadeRowCount(t, h, adv.ID)
		if rows > 0 && rows == prev {
			break
		}
		prev = rows
		time.Sleep(3 * time.Second)
	}
	if rows == 0 {
		t.Fatalf("no auction_shades rows for the advertiser after aggressive high-bid traffic — " +
			"the shade→AuctionShadeEvent→ClickHouse path isn't landing savings")
	}

	chSum := h.ClickHouseFloat(t, fmt.Sprintf(
		"SELECT sum(savings_usd) FROM adtech.auction_shades WHERE account_id='%s'", adv.ID))
	if chSum <= 0 {
		t.Fatalf("auction_shades has %d rows but SUM(savings_usd)=%v, want >0", rows, chSum)
	}

	// The portal must read that SAME total back — global (ClickHouse, not one DSP
	// pod's RAM) and STABLE across reads. Tolerance (5%) absorbs the handful of
	// shade events that can still be in-flight on a live stack; the flap bug this
	// guards against was 0-vs-full (100% off), which 5% still catches decisively.
	const tol = 0.05
	client := h.OwnerClient(t, adv.ID)
	first := gatewayShadingSavings(t, h, client)
	if first <= 0 || math.Abs(first-chSum) > tol*chSum {
		t.Fatalf("gateway shading_savings_usd=%v, want within 5%% of ClickHouse SUM(savings_usd)=%v", first, chSum)
	}
	for i := 1; i < 5; i++ {
		got := gatewayShadingSavings(t, h, client)
		if math.Abs(got-first) > tol*first {
			t.Fatalf("read %d: gateway shading_savings_usd=%v flapped from %v (>5%%) — durable read must be stable", i, got, first)
		}
	}
	t.Logf("bid-shading savings: %d auction_shades rows, SUM=$%.6f, gateway ~$%.6f (within 5%%)", rows, chSum, first)
}

// shadeRowCount returns how many auction_shades rows exist for an account.
func shadeRowCount(t *testing.T, h *harness.Harness, accountID string) int {
	t.Helper()
	return h.ClickHouseScalar(t, fmt.Sprintf(
		"SELECT count() FROM adtech.auction_shades WHERE account_id='%s'", accountID))
}

// gatewayShadingSavings GETs /v1/api/shading as the authed advertiser and returns
// the realized shading_savings_usd (the durable, ClickHouse-sourced figure).
func gatewayShadingSavings(t *testing.T, h *harness.Harness, client *http.Client) float64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.URLs.Gateway+routes.APIShading, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET shading: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET shading: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		ShadingSavingsUSD float64 `json:"shading_savings_usd"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode shading: %v (body=%s)", err, string(body))
	}
	return out.ShadingSavingsUSD
}

//go:build e2e

// Data Marketplace slice 3 — CPM-surcharge settlement. When an INTERNAL buyer
// wins an impression on a campaign targeting a segment they PURCHASED, the
// listing's CPM surcharge settles per impression through the real money spine:
//
//	debit  advertiser:{buyer}:balance            surcharge          (buyer pays)
//	credit advertiser:{seller}:balance           surcharge − margin (seller earns)
//	credit platform:marketplace_surcharge_margin margin
//
// EXACT money under the live reporting.marketplace_surcharge_margin_pct, and
// exactly-once (the marketplace_surcharge_earnings (trace,segment) PK is the
// claim — a redelivered impression can't double-settle).
package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestMarketplaceSurchargeSettlement(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	// The world's advertiser is the BUYER (owns the winning campaign + placement).
	w := harness.BuildBasicWorld(t, h, "mkt-settle")
	buyer := w.AdvAcc
	h.GrantBalance(t, buyer.ID, 100.0, "mkt-settle-fund")

	uniq := time.Now().UnixNano()
	seller := h.CreateAdvertiser(t, fmt.Sprintf("mkt-settle-seller-%d", uniq))

	// Seller lists a public segment at $0.60 CPM. The buyer purchases it.
	const cpmSurcharge = 0.60 // $0.60 CPM → 600µ per impression
	sellerSeg := h.CreatePublicSegment(t, seller.ID, "e2e-settle-seg", []string{"m1", "m2"})
	if st, body := h.MarketplaceList(t, seller.ID, sellerSeg, fmt.Sprintf("Settle Seg %d", uniq), cpmSurcharge); st != 200 {
		t.Fatalf("list: %d %s", st, body)
	}
	listingID := h.FindListingID(t, buyer.ID, fmt.Sprintf("Settle Seg %d", uniq))
	if st, body := h.MarketplacePurchase(t, buyer.ID, listingID); st != 200 {
		t.Fatalf("purchase: %d %s", st, body)
	}

	// The buyer's campaign targets the purchased segment.
	setTargeting(t, h, w.Campaign.ID, "include_segments", pq.StringArray{sellerSeg})

	// Live margin (like the data-fee test — don't hardcode 30).
	marginPct := marketplaceMarginPct(t, h)
	const surchargeMicros = 600 // 0.60 CPM / 1000
	expMargin := int64(float64(surchargeMicros) * marginPct / 100)
	expNet := int64(surchargeMicros) - expMargin

	sellerBalBefore := balanceOf(t, h, seller.ID)

	// Deliver an impression for the buyer's campaign → surcharge settles.
	trace := fmt.Sprintf("mkt-settle-%d", uniq)
	h.FireImpression(t, trace, w.Campaign.ID, w.Campaign.CreativeID, w.Placement.ID, w.Publisher.ID, buyer.ID, "USD", 3.50)

	var surSum, netSum, marginSum int64
	harness.WaitFor(t, 20*time.Second, "marketplace surcharge settled", func() bool {
		err := h.DB.QueryRow(`
SELECT COALESCE(SUM(surcharge_micros),0), COALESCE(SUM(seller_net_micros),0), COALESCE(SUM(margin_micros),0)
FROM marketplace_surcharge_earnings WHERE trace_id = $1 AND segment_id = $2`,
			trace, sellerSeg).Scan(&surSum, &netSum, &marginSum)
		return err == nil && surSum > 0
	})

	if surSum != surchargeMicros || marginSum != expMargin || netSum != expNet {
		t.Errorf("settlement micros = surcharge %d / margin %d / net %d, want %d/%d/%d",
			surSum, marginSum, netSum, surchargeMicros, expMargin, expNet)
	}

	// Seller balance grew by exactly the net (the seller runs no campaigns, so
	// their balance changes ONLY via the surcharge credit — a clean isolation).
	// The BUYER's balance is deliberately NOT asserted here: it also absorbs the
	// impression's clearing-price billing (a separate ledger entry), so its total
	// delta = impression cost + surcharge. The buyer's exact surcharge debit is
	// proven by the marketplace_surcharge ledger entries below instead.
	if delta := balanceOf(t, h, seller.ID) - sellerBalBefore; !almostEqual(delta, float64(expNet)/1e6, 1e-9) {
		t.Errorf("seller balance delta = %.9f, want %.9f", delta, float64(expNet)/1e6)
	}

	// Ledger double-entry balances: buyer debit == seller credit + platform margin
	// (scoped to reference_type='marketplace_surcharge', so isolated from the
	// impression's own billing entries).
	var debits, credits float64
	if err := h.DB.QueryRow(`
SELECT COALESCE(SUM(CASE WHEN entry_type='debit'  THEN amount END),0),
       COALESCE(SUM(CASE WHEN entry_type='credit' THEN amount END),0)
FROM ledger_entries WHERE reference_type='marketplace_surcharge' AND reference_id LIKE $1 || '%'`,
		trace).Scan(&debits, &credits); err != nil {
		t.Fatalf("ledger query: %v", err)
	}
	if !almostEqual(debits, credits, 1e-9) || !almostEqual(debits, float64(surchargeMicros)/1e6, 1e-9) {
		t.Errorf("ledger debits %.9f / credits %.9f, want both %.9f", debits, credits, float64(surchargeMicros)/1e6)
	}

	// Exactly-once: a redelivered impression must NOT double-settle.
	h.FireImpression(t, trace, w.Campaign.ID, w.Campaign.CreativeID, w.Placement.ID, w.Publisher.ID, buyer.ID, "USD", 3.50)
	time.Sleep(2 * time.Second)
	var surSum2 int64
	if err := h.DB.QueryRow(
		`SELECT COALESCE(SUM(surcharge_micros),0) FROM marketplace_surcharge_earnings WHERE trace_id = $1`, trace).Scan(&surSum2); err != nil {
		t.Fatalf("earnings recheck: %v", err)
	}
	if surSum2 != surSum {
		t.Errorf("surcharge grew on duplicate impression: %d → %d (not exactly-once)", surSum, surSum2)
	}

	// Negative: a buyer campaign that targets a NON-granted segment settles nothing.
	other := h.CreatePublicSegment(t, buyer.ID, "e2e-settle-owned", []string{"x1"})
	setTargeting(t, h, w.Campaign.ID, "include_segments", pq.StringArray{other})
	trace2 := fmt.Sprintf("mkt-settle-neg-%d", uniq)
	h.FireImpression(t, trace2, w.Campaign.ID, w.Campaign.CreativeID, w.Placement.ID, w.Publisher.ID, buyer.ID, "USD", 3.50)
	time.Sleep(2 * time.Second)
	var negCount int
	if err := h.DB.QueryRow(`SELECT count(*) FROM marketplace_surcharge_earnings WHERE trace_id = $1`, trace2).Scan(&negCount); err != nil {
		t.Fatalf("negative recheck: %v", err)
	}
	if negCount != 0 {
		t.Errorf("settled a surcharge for a non-granted targeted segment — over-charge")
	}
}

func balanceOf(t *testing.T, h *harness.Harness, accountID string) float64 {
	t.Helper()
	var b float64
	// COALESCE over a subselect so a MISSING row (fresh account, no balance yet)
	// reads 0 rather than sql.ErrNoRows.
	if err := h.DB.QueryRow(
		`SELECT COALESCE((SELECT balance FROM advertiser_balances WHERE account_id = $1::uuid), 0)`, accountID).Scan(&b); err != nil {
		t.Fatalf("balance read: %v", err)
	}
	return b
}

func marketplaceMarginPct(t *testing.T, h *harness.Harness) float64 {
	t.Helper()
	rows, err := h.DB.Query(`SELECT DISTINCT value FROM config WHERE key = 'reporting.marketplace_surcharge_margin_pct'`)
	if err != nil {
		t.Fatalf("margin config read: %v", err)
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("margin scan: %v", err)
		}
		values = append(values, v)
	}
	if len(values) > 1 {
		t.Fatalf("reporting.marketplace_surcharge_margin_pct diverges across pods (%v)", values)
	}
	if len(values) == 1 {
		f, err := strconv.ParseFloat(strings.Trim(values[0], `"`), 64)
		if err != nil {
			t.Fatalf("margin value %q: %v", values[0], err)
		}
		return f
	}
	return 30.0 // schema default
}

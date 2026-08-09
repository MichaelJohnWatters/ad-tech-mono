//go:build e2e

// Dynamic Product Ads slice 4 — per-product suppression + cross-sell. A
// SKU-carrying purchase suppresses the bought product (stops the dynamic
// creative featuring it, durably — a stale pixel can't re-add it), rotates the
// chase to the product's cross-sell complement, and KEEPS the shopper in the
// chase while items remain. A purchase that clears the cart falls through to
// whole-person suppression (the pre-DPA behaviour), unchanged for generic
// conversions.
package e2e

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestPerProductSuppressionAndCrossSell(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "dpa-sup")
	uniq := time.Now().UnixNano()
	tag := fmt.Sprintf("cart-%d", uniq)
	kibble := fmt.Sprintf("KIBBLE-%d", uniq)
	bowl := fmt.Sprintf("BOWL-%d", uniq)
	treats := fmt.Sprintf("TREATS-%d", uniq)

	// Catalog: kibble cross-sells treats.
	if _, err := h.DB.Exec(`
INSERT INTO products (account_id, sku, title, price_micros, currency, availability, complement_sku, source)
VALUES ($1::uuid, $2, 'Kibble', 38990000, 'USD', 'in_stock', $4, 'seed'),
       ($1::uuid, $3, 'Bowl',   15000000, 'USD', 'in_stock', '', 'seed'),
       ($1::uuid, $4, 'Treats',  8490000, 'USD', 'in_stock', '', 'seed')`,
		w.AdvAcc.ID, kibble, bowl, treats); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	// A single-visit retargeting segment so the shopper is enrolled (the
	// whole-person suppression path needs segment membership to observe).
	var segID string
	if err := h.DB.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, visibility, rule)
VALUES ($1::uuid, $2, 'retargeting', 'active', 'seed', 'dsp_private',
        jsonb_build_object('event','site_visit','tag',$3::text,'min_count',1))
RETURNING id::text`, w.AdvAcc.ID, fmt.Sprintf("dpa-sup-seg-%d", uniq), tag).Scan(&segID); err != nil {
		t.Fatalf("create segment: %v", err)
	}

	views := func(uid string) []string {
		got := h.ProductViewSKUs(t, w.AdvAcc.ID, uid)
		sort.Strings(got)
		return got
	}
	enrolled := func(uid string) int {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM audience_segment_members WHERE segment_id=$1 AND user_id=$2`, segID, uid).Scan(&n); err != nil {
			t.Fatalf("membership count: %v", err)
		}
		return n
	}
	waitViews := func(uid string, want int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for len(h.ProductViewSKUs(t, w.AdvAcc.ID, uid)) != want {
			if time.Now().After(deadline) {
				t.Fatalf("timed out: user %s views=%v want %d", uid, views(uid), want)
			}
			time.Sleep(400 * time.Millisecond)
		}
	}

	// --- Shopper A: multi-item cart, buys one → per-product + cross-sell, keeps chasing.
	userA := fmt.Sprintf("dpa-sup-A-%d", uniq)
	h.FireRetargetingPixelSKUs(t, w.AdvAcc.ID, userA, tag, kibble+","+bowl)
	waitViews(userA, 2)
	if enrolled(userA) == 0 {
		t.Fatalf("shopper A not enrolled in the chase segment")
	}

	h.FireConversionForVisitorSKUs(t, fmt.Sprintf("convA-%d", uniq), w.AdvAcc.ID, userA, "purchase", "USD", 38.99, kibble)
	// After buying kibble: kibble gone, bowl remains, treats cross-sell added.
	deadline := time.Now().Add(30 * time.Second)
	for {
		v := views(userA)
		if (len(v) == 2 && v[0] == bowl && v[1] == treats) || time.Now().After(deadline) {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	if got := views(userA); !(len(got) == 2 && got[0] == bowl && got[1] == treats) {
		t.Fatalf("shopper A views after purchase = %v, want [%s %s] (kibble suppressed, treats cross-sold)", got, bowl, treats)
	}
	if enrolled(userA) == 0 {
		t.Errorf("shopper A dropped from the chase after a partial purchase — should keep chasing the rest of the cart")
	}
	// The bought SKU is durably burned: a stale pixel re-fire can't re-add it.
	h.FireRetargetingPixelSKUs(t, w.AdvAcc.ID, userA, tag, kibble)
	time.Sleep(3 * time.Second)
	if strings.Contains(strings.Join(views(userA), ","), kibble) {
		t.Errorf("bought kibble re-added by a stale pixel — per-product burn not durable: %v", views(userA))
	}

	// --- Shopper B: single-item cart of a NO-complement product, buys it →
	//     cart cleared → whole-person suppression (removed from the segment).
	userB := fmt.Sprintf("dpa-sup-B-%d", uniq)
	h.FireRetargetingPixelSKUs(t, w.AdvAcc.ID, userB, tag, bowl) // bowl has no complement
	waitViews(userB, 1)
	if enrolled(userB) == 0 {
		t.Fatalf("shopper B not enrolled")
	}
	h.FireConversionForVisitorSKUs(t, fmt.Sprintf("convB-%d", uniq), w.AdvAcc.ID, userB, "purchase", "USD", 15.00, bowl)
	deadline = time.Now().Add(30 * time.Second)
	for enrolled(userB) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("shopper B still enrolled after clearing the cart — whole-person suppression didn't fire")
		}
		time.Sleep(400 * time.Millisecond)
	}
}

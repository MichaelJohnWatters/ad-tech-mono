//go:build e2e

package e2e

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestMultiSitePublishersEndToEnd is the "friends' websites" proof: several
// distinct external publisher tenants (a news site, a tech blog, a video hub),
// each running heavy multi-ad pages, with three advertiser tenants bidding
// across all of them. It asserts the things that MUST hold before real people
// rely on the platform:
//
//   - Per-publisher ZERO SLIPPAGE: every ad a site serves lands exactly one
//     impression in ClickHouse, attributed to that publisher.
//   - Reporting TENANT ISOLATION: a query scoped to one publisher returns only
//     that publisher's impressions, and the per-publisher counts sum to the
//     global total (no leakage, no double-count).
//   - Correct per-publisher REVENUE SPLIT: each publisher's net revenue is its
//     own contracted share of gross (70% / 65% / 80%), not a shared default.
//   - Every advertiser tenant BOOKS SPEND: all three advertisers win and are
//     attributed their impressions.
func TestMultiSitePublishersEndToEnd(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildMultiSiteWorld(t, h)
	from := time.Now().Add(-5 * time.Second).UTC()

	// Per-site: visit every page, prove per-trace zero slippage, and tally what
	// filled so we can check the publisher-scoped report against it.
	filledByPublisher := map[string]int{}
	globalFilled := 0

	for _, st := range w.Sites {
		siteFilled := 0
		for _, layout := range st.Site.Layouts() {
			visit := h.VisitPageWith(t, layout.Slug, st.PlacementByFormat)
			for _, sv := range visit.Slots {
				if !sv.Filled {
					continue
				}
				siteFilled++
				tid := sv.TraceID
				harness.WaitFor(t, 40*time.Second,
					fmt.Sprintf("%s: impression for %s slot (trace %s)", st.Site.Slug, sv.Format, tid),
					func() bool { return h.ImpressionsByTrace(t, tid) >= 1 })
				if got := h.ImpressionsByTrace(t, tid); got != 1 {
					t.Errorf("%s: trace %s has %d impressions, want 1 (slippage/dup)", st.Site.Slug, tid, got)
				}
			}
		}
		if siteFilled == 0 {
			t.Fatalf("site %q filled no ads — serving broken", st.Site.Slug)
		}
		filledByPublisher[st.Publisher.ID] = siteFilled
		globalFilled += siteFilled
		t.Logf("site %-10s (%s, rev %d%%): filled %d ads", st.Site.Slug, st.Publisher.ID[:8], st.Site.RevsharePct, siteFilled)
	}

	// TENANT ISOLATION + zero slippage at the reporting layer: a publisher-scoped
	// count equals exactly what that site served (retry for the async CH write),
	// and the per-publisher counts sum to the global total — nothing leaked
	// between tenants, nothing lost.
	sumScoped := 0
	for _, st := range w.Sites {
		want := filledByPublisher[st.Publisher.ID]
		filters := map[string]string{"publisher_id": st.Publisher.ID}
		harness.WaitFor(t, 40*time.Second,
			fmt.Sprintf("%s: reporting count reaches %d", st.Site.Slug, want),
			func() bool { return int(h.ReportRow(t, filters, []string{"count"}, from)["count"]) >= want })
		got := int(h.ReportRow(t, filters, []string{"count"}, from)["count"])
		if got != want {
			t.Errorf("site %q: publisher-scoped impressions = %d, want %d (isolation/slippage)", st.Site.Slug, got, want)
		}
		sumScoped += got
	}
	if globalScoped := h.ReportImpressionCountSince(t, from); sumScoped != globalScoped {
		t.Errorf("per-publisher counts sum to %d but global is %d — tenant leakage or overlap", sumScoped, globalScoped)
	}

	// REVENUE SPLIT: each publisher's net revenue is its own contracted share of
	// gross. gross = ecpm × impressions / 1000; net/gross must match RevsharePct.
	for _, st := range w.Sites {
		filters := map[string]string{"publisher_id": st.Publisher.ID}
		row := h.ReportRow(t, filters, []string{"count", "ecpm", "net_revenue"}, from)
		imps, ecpm, net := row["count"], row["ecpm"], row["net_revenue"]
		gross := ecpm * imps / 1000
		if gross <= 0 || net <= 0 {
			t.Errorf("site %q: gross=%.6f net=%.6f, want both > 0", st.Site.Slug, gross, net)
			continue
		}
		wantShare := float64(st.Site.RevsharePct) / 100
		gotShare := net / gross
		if math.Abs(gotShare-wantShare) > 0.02 {
			t.Errorf("site %q: net/gross = %.3f, want ~%.3f (revshare %d%% not applied per-tenant)",
				st.Site.Slug, gotShare, wantShare, st.Site.RevsharePct)
		} else {
			t.Logf("site %-10s revenue split: net/gross = %.2f (contracted %d%%) ✓", st.Site.Slug, gotShare, st.Site.RevsharePct)
		}
	}

	// EVERY ADVERTISER TENANT books spend: each of the three advertisers wins its
	// format across the sites and is attributed impressions.
	for name, campaign := range map[string]string{
		"acme(display)":  w.AcmeDisplayCampaign,
		"acme(native)":   w.AcmeNativeCampaign,
		"globex(video)":  w.GlobexVideoCampaign,
		"initech(audio)": w.InitechAudioCampaign,
	} {
		filters := map[string]string{"campaign_id": campaign}
		harness.WaitFor(t, 40*time.Second, fmt.Sprintf("advertiser %s books impressions", name),
			func() bool { return int(h.ReportRow(t, filters, []string{"count"}, from)["count"]) >= 1 })
		if got := int(h.ReportRow(t, filters, []string{"count"}, from)["count"]); got < 1 {
			t.Errorf("advertiser campaign %s booked %d impressions, want >= 1", name, got)
		}
	}
}

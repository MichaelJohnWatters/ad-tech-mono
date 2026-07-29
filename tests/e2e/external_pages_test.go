//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/simulator/pages"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// TestExternalPublisherPagesFlowToClickHouse visits every external-publisher
// page layout (pkg/simulator/pages — the same layouts cmd/demosite renders) the
// way a browser would: for each ad slot it serves the real /v1/pubad/* endpoint
// and fires the served ad's own signed impression + viewability beacons. It then
// asserts ZERO SLIPPAGE — every slot that filled lands exactly one impression in
// ClickHouse, and every viewable slot lands exactly one viewable view.
//
// This is the multi-slot, multi-format twin of the single-serve billing tests:
// it proves a real 3-ad or 6-ad page delivers all of its ads downstream, across
// display / native / video / audio combined on one page.
func TestExternalPublisherPagesFlowToClickHouse(t *testing.T) {
	h := harness.New(t)
	h.RefreshAllCaches(t)

	for _, layout := range pages.All() {
		layout := layout
		t.Run(layout.Slug, func(t *testing.T) {
			visit := h.VisitPage(t, layout.Slug)

			filled := visit.FilledTraces()
			if len(filled) == 0 {
				t.Fatalf("page %q: no slot filled — serving path broken", layout.Slug)
			}
			t.Logf("page %q: %d/%d slots filled", layout.Slug, len(filled), layout.Total())

			// Zero slippage: each filled slot's trace must land EXACTLY one
			// impression. Retry per trace for the async NATS→reporting→CH write.
			for _, sv := range visit.Slots {
				if !sv.Filled {
					// An unfilled slot is a fill gap, not a slippage bug; log it
					// so a page that silently stops filling a format is visible.
					t.Logf("  slot %-7s %-18s: no fill", sv.Format, sv.Label)
					continue
				}
				tid := sv.TraceID
				if tid == "" {
					t.Errorf("  slot %s %q filled but served no trace_id", sv.Format, sv.Label)
					continue
				}
				harness.WaitFor(t, 40*time.Second,
					fmt.Sprintf("impression for %s slot %q (trace %s)", sv.Format, sv.Label, tid),
					func() bool { return h.ImpressionsByTrace(t, tid) >= 1 })
				if got := h.ImpressionsByTrace(t, tid); got != 1 {
					t.Errorf("  slot %s %q: impressions for trace %s = %d, want 1 (slippage/dup)", sv.Format, sv.Label, tid, got)
				}

				// Viewable formats (display/video): the viewability beacon we
				// fired must reach the views table as an IAB-viewable row.
				if sv.FiredView && sv.Viewable {
					harness.WaitFor(t, 40*time.Second,
						fmt.Sprintf("viewable view for %s slot %q (trace %s)", sv.Format, sv.Label, tid),
						func() bool { return h.ViewableViewsByTrace(t, tid) >= 1 })
					if got := h.ViewableViewsByTrace(t, tid); got != 1 {
						t.Errorf("  slot %s %q: viewable views for trace %s = %d, want 1", sv.Format, sv.Label, tid, got)
					}
				}
			}

			// Aggregate anti-slippage: the number of distinct impression traces
			// downstream equals the number of slots that filled — nothing extra,
			// nothing lost.
			total := 0
			for _, tid := range filled {
				total += h.ImpressionsByTrace(t, tid)
			}
			if total != len(filled) {
				t.Errorf("page %q: %d impressions downstream for %d filled slots (want equal)", layout.Slug, total, len(filled))
			}
		})
	}
}

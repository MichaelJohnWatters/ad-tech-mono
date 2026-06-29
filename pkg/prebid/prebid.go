// Package prebid provides the translation layer between Prebid Server
// (prebid.org) bid requests and our internal openrtb.BidRequest. We act
// as a Prebid-compatible bidder: external Prebid Server instances send us
// OpenRTB 2.x requests at /v1/prebid/openrtb2/auction, we run them through
// our auction, and return OpenRTB-shape responses.
//
// Bidder code: "adtechmono" (the unique short identifier in any future
// Prebid registry submission and in our public Prebid-compatible endpoint
// docs). See docs/PLAN.md → "Prebid Server Integration" for the decision
// rationale.
//
// What this package does:
//   - Translation isn't really needed at the type level — Prebid speaks
//     OpenRTB 2.x and so do we — but the *contract* differs: we honour
//     publisher-side dynamic floors that Prebid sends, and we layer our
//     own floor on top (max wins).
//   - Logs inbound deal IDs opaquely; no attempt to map them to internal
//     pkg/deals (per the design decision).
//
// What this package does NOT do (intentionally, per starting scope):
//   - Translate Prebid `ext.prebid.*` server-to-server-only extensions.
//     We're a bidder, not a Prebid Server host; those fields are between
//     the publisher's Prebid setup and itself.
//   - Cookie sync. That lives in cmd/exchange's setuid handler, which
//     consumes pkg/identity to map publisher-side IDs to our internal IDs.
package prebid

import (
	"log/slog"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// BidderCode is our registered short identifier in any future Prebid
// registry submission and in our public Prebid bidder docs.
const BidderCode = "adtechmono"

// ResolveFloor returns the effective per-impression floor for a Prebid
// inbound request. Effective floor = max(inbound bidfloor, our minimum).
//
// Honours the publisher's dynamic-floor logic (already applied by their
// Prebid Server) while still enforcing our own platform-wide minimum
// (configurable via exchange.min_bid_floor). Matches the AppNexus /
// PubMatic / Prebid Server reference behaviour.
func ResolveFloor(prebidBidFloor, ourMinFloor float64) float64 {
	if ourMinFloor > prebidBidFloor {
		return ourMinFloor
	}
	return prebidBidFloor
}

// ApplyFloorPolicy mutates the request in place, raising any per-impression
// bidfloor below ourMinFloor up to ourMinFloor. Logs the inbound deal IDs
// for observability (we accept them opaquely; matching against internal
// deals is deferred).
//
// Returns the count of impressions whose floor was raised, for metrics.
func ApplyFloorPolicy(req *openrtb.BidRequest, ourMinFloor float64, log *slog.Logger) int {
	raised := 0
	for i := range req.Imp {
		original := req.Imp[i].BidFloor
		effective := ResolveFloor(original, ourMinFloor)
		if effective != original {
			req.Imp[i].BidFloor = effective
			raised++
		}
		if req.Imp[i].DealID != "" && log != nil {
			log.Info("prebid inbound deal id (opaque pass-through)",
				"imp_id", req.Imp[i].ID,
				"deal_id", req.Imp[i].DealID,
			)
		}
	}
	return raised
}

package privacy

import "strings"

// Data residency (PLAN Phase 11 #111), bid-time / data-plane half. A bid request
// may assert a data-residency region via regs.ext.data_residency. When it names a
// region that is NOT this deployment's home region, the request's user-level data
// belongs in that region's infrastructure and must not be stored or emitted here
// (identity-graph edges, behaviour/audience enrolment, audience segments leaving
// on the bid request). Bidding itself is unaffected — like the consent gate,
// residency only governs whether we RETAIN user data, never whether we bid.

// AllowsResidency reports whether user-level data for a request asserting
// requestRegion may be stored/emitted in a deployment whose home region is
// homeRegion. An empty requestRegion (no assertion) or empty homeRegion imposes no
// residency constraint — the permissive default, matching single-region deploys.
// Comparison is case- and whitespace-insensitive: region tokens arrive from a
// query param, a config value, and a JWT, and a stray "US-East-1"/" us-east-1"
// mismatch must NOT silently flip every account out-of-region (a deployment-wide
// lockout). This is THE single residency rule — the gateway control-plane gate
// (middleware.Auth) calls it too, so the two planes can't drift.
func AllowsResidency(requestRegion, homeRegion string) bool {
	rq, hm := normRegion(requestRegion), normRegion(homeRegion)
	return rq == "" || hm == "" || rq == hm
}

func normRegion(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// AllowsUserData combines the consent decision with the residency gate: user-level
// data may be stored/emitted only when personalisation is consented AND residency
// permits storing it in this region. Whether we BID is governed by Decision.Bid,
// not this.
func AllowsUserData(d Decision, requestRegion, homeRegion string) bool {
	return d.Personalise && AllowsResidency(requestRegion, homeRegion)
}

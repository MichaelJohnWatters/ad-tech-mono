//go:build e2e

// G7: per-advertiser conversion signing keys. A conversion is the CPA billing
// trigger, so /v1/t/conv must be validated against the SIGNING ADVERTISER's own
// HMAC key (by advid) — not one shared platform key that every advertiser holds.
// This proves the cross-advertiser forgery hole is closed: once advertiser A has
// its own key and strict mode is on, a conversion billed to A signed with the
// shared platform key (as a party holding it would forge) is REJECTED and never
// enters the pipeline, while A's own key is accepted and recorded.
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestConversionPerAdvertiserKeyRejectsForgery(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "g7-key")
	acctA := w.AdvAcc.ID

	// Advertiser A's own conversion-signing key (what its server signs postbacks
	// with). The shared platform key (DefaultSigningKey) stands in for "some other
	// party holding the shared key" — the forgery vector.
	const advAKey = "adv-A-conversion-key-g7-e2e"
	h.IssueConversionKey(t, acctA, advAKey)

	const pod = "tracker-0"
	const sigKey = "tracker.signature_validation"
	const strictKey = "tracker.conversion_strict_advertiser_key"
	// Revert BOTH knobs to their DEPLOYED baselines on the way out (helm sets
	// both TRACKER_SIGNATURE_VALIDATION and TRACKER_CONVERSION_STRICT_ADVERTISER_KEY
	// to true). DELETE the overrides rather than forcing "false" — restoring the
	// schema default downgraded the prod-shaped stack and poisoned later tests
	// that assume strict signing/keys are on (same discipline as
	// TestTrackerHMACStrictMode).
	t.Cleanup(func() {
		h.DeleteConfig(t, strictKey)
		h.DeleteConfig(t, sigKey)
	})
	h.SetConfigForPod(t, sigKey, "true", pod)
	h.SetConfigForPod(t, strictKey, "true", pod)

	// Gate on the FORGERY being rejected. This condition is only true once (a)
	// signature_validation propagated, (b) strict propagated, AND (c) the
	// tracker's warm cache actually loaded A's key — until A has a key the tracker
	// falls back to the platform key and the "forged" postback would be accepted.
	// Fresh trace ids each attempt keep dedup out of it.
	harness.WaitFor(t, 40*time.Second, "shared-platform-key conversion for A is rejected (forgery blocked)", func() bool {
		tid := fmt.Sprintf("g7-forge-probe-%d", time.Now().UnixNano())
		return h.FireConversionSignedStatus(t, tid, acctA, "vis-A", "purchase", "USD", 19.99, adserving.DefaultSigningKey) == 403
	})

	// 1) FORGERY: a conversion billed to A, signed with the shared platform key,
	//    is rejected AND never recorded — so it can drive no CPA spend.
	forgeTrace := fmt.Sprintf("g7-forge-%d", time.Now().UnixNano())
	if st := h.FireConversionSignedStatus(t, forgeTrace, acctA, "vis-A", "purchase", "USD", 19.99, adserving.DefaultSigningKey); st != 403 {
		t.Fatalf("forged conversion (shared key, advid=A) got status %d, want 403", st)
	}

	// 2) LEGITIMATE: the same conversion signed with A's OWN key is accepted.
	okTrace := fmt.Sprintf("g7-ok-%d", time.Now().UnixNano())
	if st := h.FireConversionSignedStatus(t, okTrace, acctA, "vis-A", "purchase", "USD", 19.99, advAKey); st != 200 {
		t.Fatalf("legitimate conversion (A's own key, advid=A) got status %d, want 200", st)
	}

	// The accepted conversion enters the pipeline (a billable row); the forged one
	// never does. This is the billing-relevant proof: forgery yields no conversion
	// to settle against, A's own key yields exactly one.
	waitCH(t, h, fmt.Sprintf("SELECT count() FROM adtech.conversions WHERE trace_id='%s'", okTrace), "accepted conversion recorded")
	if got := h.ClickHouseScalar(t, fmt.Sprintf("SELECT count() FROM adtech.conversions WHERE trace_id='%s'", forgeTrace)); got != 0 {
		t.Errorf("forged conversion produced %d rows in ClickHouse; want 0 (must never bill)", got)
	}
}

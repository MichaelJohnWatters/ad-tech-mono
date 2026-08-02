//go:build e2e

package harness

import (
	"context"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

// FireImpression hits the tracker's impression pixel endpoint as if a
// browser had loaded the served creative. The trace_id, campaign_id and
// clearing price come from the auction result so the NATS event chain
// matches what production would emit. Defaults bid_model to CPM —
// callers that need to exercise CPC/CPA/vCPM settle paths use
// FireImpressionWithModel instead.
func (h *Harness) FireImpression(t *testing.T, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, currency string, price float64) {
	t.Helper()
	h.FireImpressionWithModel(t, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, currency, price, "cpm")
}

// FireImpressionWithModel is FireImpression with an explicit bid_model
// query param. Production builds the impression URL via
// pkg/adserving.BuildImpressionURL which already stamps `bm`; tests
// bypass that path and must declare the model themselves so the billing
// engine routes reserve-vs-bill-immediately correctly.
func (h *Harness) FireImpressionWithModel(t *testing.T, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, currency string, price float64, bidModel string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/imp?tid=%s&cid=%s&crid=%s&pid=%s&pubid=%s&advid=%s&price=%.4f&cur=%s&bm=%s",
		h.URLs.Tracker, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, price, currency, bidModel)
	h.fireAndConsume(t, url, "impression")
}

// FireImpressionWithUser is FireImpressionWithModel that also carries the
// consented visitor id (uid) — the signal that makes the tracker write a
// behaviour_signals kind='impression' row (user_id + advertiser account), the
// lookback source view-through attribution reads. Production stamps uid on the
// beacon only for consented serves (ServeRequest.BehaviourUserID).
func (h *Harness) FireImpressionWithUser(t *testing.T, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, currency string, price float64, bidModel, userID string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/imp?tid=%s&cid=%s&crid=%s&pid=%s&pubid=%s&advid=%s&price=%.4f&cur=%s&bm=%s&uid=%s",
		h.URLs.Tracker, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, price, currency, bidModel, userID)
	h.fireAndConsume(t, url, "impression")
}

// FireConversionForVisitorCampaign is FireConversionForVisitor that also names
// the campaign (cid) up front — so the attributor knows the campaign before the
// lookback and can apply that campaign's per-line-item attribution overrides.
func (h *Harness) FireConversionForVisitorCampaign(t *testing.T, convTrace, accountID, campaignID, uid, convType, currency string, revenue float64) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/conv?tid=%s&type=%s&rev=%.4f&cur=%s&advid=%s&cid=%s&uid=%s",
		h.URLs.Tracker, convTrace, convType, revenue, currency, accountID, campaignID, uid)
	h.fireAndConsume(t, url, "conversion")
}

// FireImpressionWithUserHH is FireImpressionWithUser that also carries the
// household id (hh) the ad server now bakes onto consented beacons — so the
// behaviour_signals row gets household_id, the fallback key view-through
// attribution matches when the exact user id doesn't line up.
func (h *Harness) FireImpressionWithUserHH(t *testing.T, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, currency string, price float64, bidModel, userID, household string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/imp?tid=%s&cid=%s&crid=%s&pid=%s&pubid=%s&advid=%s&price=%.4f&cur=%s&bm=%s&uid=%s&hh=%s",
		h.URLs.Tracker, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, price, currency, bidModel, userID, household)
	h.fireAndConsume(t, url, "impression")
}

// FireConversionForVisitor fires a click-less conversion the way an advertiser's
// server-to-server postback does for a view-through: its own (synthetic) trace,
// the advertiser account (advid) and the advertiser-side visitor id (uid), and
// NO ctid (no click to thread). Attribution must resolve the visitor to a prior
// viewable exposure. Signs like production.
func (h *Harness) FireConversionForVisitor(t *testing.T, convTrace, accountID, uid, convType, currency string, revenue float64) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/conv?tid=%s&type=%s&rev=%.4f&cur=%s&advid=%s&uid=%s",
		h.URLs.Tracker, convTrace, convType, revenue, currency, accountID, uid)
	h.fireAndConsume(t, url, "conversion")
}

// IssueConversionKey inserts a per-advertiser hmac_conversion signing key (G7)
// straight into the secrets table and pokes the tracker's warm cache to reload.
// The e2e stack runs the secrets cipher in passthrough mode (no
// SECRETS_ENCRYPTION_KEY), so the value is stored + read as plaintext — the same
// value the caller signs conversion postbacks with. Scoped to accountID
// (owner=platform, account-scoped), so the tracker validates ONLY this
// advertiser's conversions against it. Idempotent across reruns.
func (h *Harness) IssueConversionKey(t *testing.T, accountID, value string) {
	t.Helper()
	if _, err := h.DB.Exec(`DELETE FROM secrets WHERE purpose='hmac_conversion' AND account_id=$1`, accountID); err != nil {
		t.Fatalf("clear prior conversion key for %s: %v", accountID, err)
	}
	if _, err := h.DB.Exec(
		`INSERT INTO secrets (name, value, purpose, owner, account_id, status)
		 VALUES ($1, $2, 'hmac_conversion', 'platform', $3, 'active')`,
		"e2e-convkey-"+accountID, value, accountID); err != nil {
		t.Fatalf("issue conversion key for %s: %v", accountID, err)
	}
	// Nudge the tracker's secrets warm cache to reload now rather than waiting
	// for its 30s poll. The WaitFor in the test still covers the reload latency.
	h.PublishInvalidate(t, events.SubjectCacheInvalidateSecrets)
}

// FireConversionSignedStatus fires an S2S conversion postback signed with a
// SPECIFIC key and returns the HTTP status (it does NOT consume NATS). advid is
// the billed advertiser; signingKey is what the caller signs with. Per-advertiser
// validation (G7): the advertiser's OWN key is accepted; any other key (e.g. the
// shared platform key held by a different advertiser) is a forgery and rejected
// under tracker.conversion_strict_advertiser_key. The sig covers the full query,
// exactly as demoadv's server-to-server postback signs it.
func (h *Harness) FireConversionSignedStatus(t *testing.T, convTrace, accountID, uid, convType, currency string, revenue float64, signingKey string) int {
	t.Helper()
	base := fmt.Sprintf("%s/v1/t/conv?tid=%s&type=%s&rev=%.4f&cur=%s&advid=%s&uid=%s",
		h.URLs.Tracker, convTrace, convType, revenue, currency, accountID, uid)
	signed := adserving.SignURL(base, signingKey)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, signed, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (e2e-harness)")
	req.Header.Set("Referer", "https://e2e.test/")
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("fire signed conversion: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// FireRetargetingPixel fires the advertiser retargeting pixel (/v1/t/rt) — the
// buy-side, unsigned pixel adtech-adv.js fires on a consented visit. With a
// hashed email it also asks the tracker to link the advertiser visitor id to
// that email in the identity graph (the bridge for view-through). No signing
// (a third-party page can't sign); no privacy params = consented.
func (h *Harness) FireRetargetingPixel(t *testing.T, accountID, uid, hashedEmail, tag string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/rt?uid=%s&aid=%s&tag=%s&tid=rt-%d",
		h.URLs.Tracker, uid, accountID, tag, time.Now().UnixNano())
	if hashedEmail != "" {
		url += "&he=" + hashedEmail
	}
	h.fireRawReturningHeader(t, url, "retarget", "")
}

// FireImpressionDeal is FireImpression with the winning deal's ID stamped
// as the `deal` query param — production stamps it via
// pkg/adserving.BuildImpressionURL when the serve context carries a deal.
// Reporting resolves the id to its deal TYPE (pg/pmp/preferred/open) so
// billing can apply the contract's deal_type_modifiers.
func (h *Harness) FireImpressionDeal(t *testing.T, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, currency string, price float64, dealID string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/imp?tid=%s&cid=%s&crid=%s&pid=%s&pubid=%s&advid=%s&price=%.4f&cur=%s&bm=cpm&deal=%s",
		h.URLs.Tracker, traceID, campaignID, creativeID, placementID, publisherID, advertiserID, price, currency, dealID)
	h.fireAndConsume(t, url, "impression")
}

// FireClick hits the click redirect endpoint. Skips following the redirect
// since the e2e test only cares about the pixel firing and the NATS event,
// not the landing page.
func (h *Harness) FireClick(t *testing.T, traceID, campaignID, redir string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/click?tid=%s&cid=%s&redir=%s",
		h.URLs.Tracker, traceID, campaignID, redir)
	h.fireAndConsume(t, url, "click")
}

// FireView hits the viewability beacon with measurement params. The tracker
// computes IAB viewability server-side from dur/pct/areaPx; areaPx=0 uses
// the default 50% threshold. Tests assert the server's verdict via the
// returned X-IAB-Viewable header (or, for vCPM, by checking that the
// reservation did/didn't settle in the billing ledger).
func (h *Harness) FireView(t *testing.T, traceID, campaignID, placementID, publisherID string, durMs, pct int, areaPx int64) bool {
	t.Helper()
	return h.fireViewMeasured(t, h.viewBase(traceID, campaignID, placementID, publisherID, ""), durMs, pct, areaPx)
}

// FireVideoView is FireView for the VIDEO channel — the beacon carries a signed
// ch=video, so the tracker applies the 2s IAB dwell (vs 1s for display). Used to
// prove video viewability + video vCPM settle.
func (h *Harness) FireVideoView(t *testing.T, traceID, campaignID, placementID, publisherID string, durMs, pct int, areaPx int64) bool {
	t.Helper()
	return h.fireViewMeasured(t, h.viewBase(traceID, campaignID, placementID, publisherID, "video"), durMs, pct, areaPx)
}

func (h *Harness) viewBase(traceID, campaignID, placementID, publisherID, channel string) string {
	u := fmt.Sprintf("%s/v1/t/view?tid=%s&cid=%s&pid=%s&pubid=%s",
		h.URLs.Tracker, traceID, campaignID, placementID, publisherID)
	if channel != "" {
		u += "&ch=" + channel
	}
	return u
}

// fireViewMeasured signs the BASE view URL (tid/cid/pid/pubid[/ch]) — exactly
// what the ad server signs — then appends the CLIENT-measured dur/pct/area,
// which the tracker excludes from signature validation (viewSigParams). Signing
// the whole thing (measurement included) would 403 under strict signing.
func (h *Harness) fireViewMeasured(t *testing.T, base string, durMs, pct int, areaPx int64) bool {
	t.Helper()
	signed := adserving.SignURL(base, adserving.DefaultSigningKey)
	url := fmt.Sprintf("%s&dur=%d&pct=%d&area=%d", signed, durMs, pct, areaPx)
	return h.fireRawReturningHeader(t, url, "view", "X-IAB-Viewable") == "1"
}

// FireImpressionURL fires an ALREADY-SIGNED /v1/t/imp URL — the exact
// impression pixel the ad server minted and embedded in the served creative /
// VAST. Does NOT re-sign (the URL already carries the ad server's HMAC).
// Proves a served ad's own impression URL records an impression end to end.
func (h *Harness) FireImpressionURL(t *testing.T, url string) {
	t.Helper()
	h.fireRawReturningHeader(t, url, "impression", "")
}

// FireViewURL fires an ALREADY-SIGNED /v1/t/view URL (e.g. the signed
// viewability URL a served ad injects, with the client-measured dur/pct/area
// appended) and reports whether the tracker judged it IAB-viewable. Does NOT
// re-sign — the URL already carries the ad server's HMAC; re-signing would add a
// second sig param and invalidate it. Proves an injected beacon's URL produces
// a real view end to end.
func (h *Harness) FireViewURL(t *testing.T, url string) bool {
	t.Helper()
	return h.fireRawReturningHeader(t, url, "view", "X-IAB-Viewable") == "1"
}

func (h *Harness) fireAndConsumeReturningHeader(t *testing.T, url, eventKind, header string) string {
	// Sign like production: tracker.signature_validation is enforced on the
	// local stack (2026-07-19 ratchet), so hand-built beacons must carry a
	// valid HMAC exactly as adserver-built ones do.
	return h.fireRawReturningHeader(t, adserving.SignURL(url, adserving.DefaultSigningKey), eventKind, header)
}

// fireRawReturningHeader fires url AS-IS (no signing) and returns the given
// response header — for URLs the ad server already signed.
func (h *Harness) fireRawReturningHeader(t *testing.T, url, eventKind, header string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("fire %s: %v", eventKind, err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (e2e-harness)")
	req.Header.Set("Referer", "https://e2e.test/")
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatalf("fire %s call: %v", eventKind, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 500 {
		t.Fatalf("fire %s status %d", eventKind, resp.StatusCode)
	}
	return resp.Header.Get(header)
}

// FireConversion hits the conversion pixel endpoint for the given type and
// revenue. Used to test CPA settle flows.
func (h *Harness) FireConversion(t *testing.T, traceID, campaignID, convType, currency string, revenue float64) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/conv?tid=%s&cid=%s&type=%s&rev=%.4f&cur=%s",
		h.URLs.Tracker, traceID, campaignID, convType, revenue, currency)
	h.fireAndConsume(t, url, "conversion")
}

// FireConversionAttributed models the REAL deterministic click-through path (the
// demo advertiser's signed S2S postback shape): the conversion's OWN trace
// (convTrace, e.g. a synthetic order id) is distinct from the earning exposure's
// trace, which rides as the signed ctid. Settlement must credit the ctid's
// reservation, not the conversion's own trace. Both ride inside the HMAC.
func (h *Harness) FireConversionAttributed(t *testing.T, convTrace, attributedTrace, campaignID, convType, currency string, revenue float64) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/conv?tid=%s&cid=%s&type=%s&rev=%.4f&cur=%s&ctid=%s",
		h.URLs.Tracker, convTrace, campaignID, convType, revenue, currency, attributedTrace)
	h.fireAndConsume(t, url, "conversion")
}

// noRedirectClient is a request-scoped HTTP client that stops at the
// first 3xx so /v1/t/click (which 302s to the landing URL) doesn't end
// up dialling a fake domain like landing.test. Pre-existing h.HTTP
// follows redirects, which is the right default for ad-serve but wrong
// for the click tracker.
var noRedirectClient = &http.Client{
	Timeout:   5 * time.Second,
	Transport: retryTransport{}, // survive port-forward flaps under full-suite load
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (h *Harness) fireAndConsume(t *testing.T, url, eventKind string) {
	// Sign like production: tracker.signature_validation is enforced on the
	// local stack (2026-07-19 ratchet), so hand-built beacons must carry a
	// valid HMAC exactly as adserver-built ones do. A CONVERSION postback is the
	// CPA billing trigger and is validated PER-ADVERTISER under the prod-shaped
	// tracker.conversion_strict_advertiser_key (G7): sign it with the advertiser's
	// own deterministic dev key (matches the seed / createAccount mint). advid=""
	// → DevConversionKey returns the platform key, which the tracker accepts for
	// advertisers with no issued key. Impression/click/view stay on the platform
	// key (the ad server signs those).
	signKey := adserving.DefaultSigningKey
	if eventKind == "conversion" {
		if u, err := neturl.Parse(url); err == nil {
			signKey = adserving.DevConversionKey(u.Query().Get("advid"))
		}
	}
	url = adserving.SignURL(url, signKey)
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("fire %s: %v", eventKind, err)
	}
	// The tracker's real-time fraud check flags the default Go UA as a bot
	// (score 0.9, reason "bot_user_agent") and silently drops the event —
	// the pixel still returns 200 but no NATS publish happens, so billing
	// never accrues. Set a browser-shaped UA and a Referer so the harness
	// looks like a real impression. Without this, every tracker-driven
	// assertion silently passes by accident.
	req.Header.Set("User-Agent", "Mozilla/5.0 (e2e-harness)")
	req.Header.Set("Referer", "https://e2e.test/")
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatalf("fire %s call: %v", eventKind, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 500 {
		t.Fatalf("fire %s status %d", eventKind, resp.StatusCode)
	}
}

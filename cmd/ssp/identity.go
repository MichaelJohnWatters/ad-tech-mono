package main

import (
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identityobserve"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// identityPublisher observes the identity signals on an ad-tag request and
// publishes them (via the shared identityobserve.Publisher) to the
// identity-consumer, which builds graph edges. The SSP does no writing itself,
// so the write + global probabilistic state live in one consumer. Nil is a no-op.
type identityPublisher struct {
	pub *identityobserve.Publisher
}

func newIdentityPublisher(bus events.EventBus, log *slog.Logger) *identityPublisher {
	p := identityobserve.NewPublisher(bus, log)
	if p == nil {
		return nil
	}
	return &identityPublisher{pub: p}
}

// Observe publishes the identifiers (and IP+UA fingerprint) seen on this
// request. Fire-and-forget; safe on a nil receiver. householdID (may be "")
// is the platform household id derived by the caller — including it links
// user_id/uid2/device ↔ household in the identity graph, which is what
// makes cross-device household resolution possible. endUserIP is the caller-
// resolved end-user IP (newEndUserIPFn) — the SAME address household
// derivation used, so the fingerprint and the household can never disagree.
func (p *identityPublisher) Observe(r *http.Request, userID, uid2, householdID, endUserIP string) {
	if p == nil {
		return
	}
	// Consent gate (mirrors behaviourPublisher.Observe): building identity-graph
	// edges links a user's identifiers, so it needs personalisation consent — the
	// same gate the behaviour publish right after this uses. Without it, a
	// GDPR-no-consent / GPC / opt-out serve would still silently build the graph
	// (a privacy gap, and — since attribution now reads the graph for billing — a
	// poisoning surface from unauthenticated browser serves).
	if !requestConsent(r).Personalise {
		return
	}
	// Everything request-derived is extracted HERE, synchronously — r is dead
	// once the handler returns. The publish itself moves off the serve path:
	// Publisher.Publish is a JetStream publish that BLOCKS for the broker ack
	// (up to 1s), and phase profiling showed it as most of pre_auction's
	// non-segment cost under load (~38ms p50 at 110rps — the bus is busy with
	// impression/auction events). Graph edges are best-effort observability;
	// the auction must not wait on them. Mirrors behaviourPublisher.Observe.
	traceID := tracing.TraceIDFromContext(r.Context())
	ids := gatherSignals(r, userID, uid2, householdID)
	fp := requestFingerprint(r, endUserIP)
	go p.pub.Publish(traceID, ids, fp)
}

// gatherSignals collects the distinct identifiers present on a request, in a
// fixed order, dropping duplicates and empties.
func gatherSignals(r *http.Request, userID, uid2, householdID string) []identityobserve.Signal {
	q := r.URL.Query()
	candidates := []identityobserve.Signal{
		{Value: userID, Source: identity.SourcePublisherUserID},
		{Value: uid2, Source: identity.SourceUID2},
		{Value: q.Get("hashed_email"), Source: identity.SourceHashedEmail},
		{Value: q.Get("ifa"), Source: identity.SourceDeviceID},
		{Value: q.Get("publisher_user_id"), Source: identity.SourcePublisherUserID},
		{Value: householdID, Source: identity.SourceHousehold},
	}
	seen := make(map[string]struct{}, len(candidates))
	out := make([]identityobserve.Signal, 0, len(candidates))
	for _, c := range candidates {
		if c.Value == "" {
			continue
		}
		if _, dup := seen[c.Value]; dup {
			continue
		}
		seen[c.Value] = struct{}{}
		out = append(out, c)
	}
	return out
}

// requestFingerprint is the end user's IP + user-agent for probabilistic
// matching. The IP is the caller-resolved end-user IP (trusted-proxy parse +
// allowlist-gated ?ip= override — newEndUserIPFn); the UA prefers explicit
// ?ua (forwarded by the ad tag), then the User-Agent header. Empty when
// either half is missing (we never fingerprint on IP alone).
func requestFingerprint(r *http.Request, endUserIP string) string {
	ua := r.URL.Query().Get("ua")
	if ua == "" {
		ua = r.Header.Get("User-Agent")
	}
	if endUserIP == "" || ua == "" {
		return ""
	}
	return endUserIP + "|" + ua
}

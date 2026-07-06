package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identityobserve"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// identityPublisher extracts the identity signals on a request and publishes an
// ObservedEvent to NATS, where the identity-consumer builds graph edges. The
// SSP does no writing itself — it's cheap and best-effort here so the write
// (and the global probabilistic state) live in one consumer, off every serving
// pod. A nil publisher is a no-op.
type identityPublisher struct {
	bus events.EventBus
	log *slog.Logger
}

func newIdentityPublisher(bus events.EventBus, log *slog.Logger) *identityPublisher {
	if bus == nil {
		return nil
	}
	return &identityPublisher{bus: bus, log: log}
}

// Observe publishes the identifiers (and IP+UA fingerprint) seen on this
// request. Fire-and-forget; safe on a nil receiver.
func (p *identityPublisher) Observe(r *http.Request, userID, uid2 string) {
	if p == nil {
		return
	}
	ids := gatherSignals(r, userID, uid2)
	fp := requestFingerprint(r)
	// Nothing to link from: <2 ids and no fingerprint.
	if len(ids) < 2 && (len(ids) == 0 || fp == "") {
		return
	}
	payload, err := identityobserve.Marshal(identityobserve.ObservedEvent{
		TraceID:     tracing.TraceIDFromContext(r.Context()),
		IDs:         ids,
		Fingerprint: fp,
	})
	if err != nil {
		return
	}
	// Short, independent context so a finished request doesn't cancel the publish.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.bus.Publish(ctx, events.SubjectIdentityObserved, payload); err != nil {
		p.log.Debug("identity observation publish failed (best-effort)", "error", err)
	}
}

// gatherSignals collects the distinct identifiers present on a request, in a
// fixed order, dropping duplicates and empties.
func gatherSignals(r *http.Request, userID, uid2 string) []identityobserve.Signal {
	q := r.URL.Query()
	candidates := []identityobserve.Signal{
		{Value: userID, Source: identity.SourcePublisherUserID},
		{Value: uid2, Source: identity.SourceUID2},
		{Value: q.Get("hashed_email"), Source: identity.SourceHashedEmail},
		{Value: q.Get("ifa"), Source: identity.SourceDeviceID},
		{Value: q.Get("publisher_user_id"), Source: identity.SourcePublisherUserID},
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
// matching. Prefers explicit ?ip / ?ua (forwarded by the ad tag), then falls
// back to X-Forwarded-For / User-Agent. Empty when either half is missing (we
// never fingerprint on IP alone).
func requestFingerprint(r *http.Request) string {
	q := r.URL.Query()
	ip := q.Get("ip")
	if ip == "" {
		ip = clientIP(r)
	}
	ua := q.Get("ua")
	if ua == "" {
		ua = r.Header.Get("User-Agent")
	}
	if ip == "" || ua == "" {
		return ""
	}
	return ip + "|" + ua
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

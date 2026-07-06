package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// identityWriter is the write dependency of the observer — satisfied by
// *postgres.Store (LinkIdentity).
type identityWriter interface {
	LinkIdentity(ctx context.Context, edges []postgres.IdentityEdge) (int, error)
}

// identityObserver auto-builds the identity graph from inbound requests:
//
//   - Deterministic: when a request carries two or more identifiers (user_id,
//     uid2, hashed_email, device ifa) those are the same person — edges at
//     confidence 1.0.
//   - Probabilistic (opt-in): different users seen from the same IP + user-agent
//     across requests are *likely* the same person/device — edges at a lower,
//     configurable confidence. Kept conservative: exact IP+UA match, and a
//     fingerprint seen with more than fpMaxUsers distinct ids is treated as a
//     shared IP and not linked.
//
// It stays off the hot path: Observe enqueues onto a buffered channel (dropping
// if full); a single background goroutine owns all state (dedup set + the
// probabilistic fingerprint buckets, so no locking) and flushes new edges to
// Postgres in batches. A nil *identityObserver is a no-op.
type identityObserver struct {
	writer      identityWriter
	log         *slog.Logger
	ch          chan observation
	flush       time.Duration
	seenCap     int
	maxBatch    int
	probEnabled bool
	probConf    float64
	fpMaxUsers  int
	stop        chan struct{}
	done        chan struct{}
}

// observation is one unit of work on the channel: either pre-built deterministic
// edges, or a probabilistic (id, fingerprint) sighting to be bucketed.
type observation struct {
	edges  []postgres.IdentityEdge
	probID string
	fp     string
}

func newIdentityObserver(writer identityWriter, flush time.Duration, seenCap int, probEnabled bool, probConf float64, fpMaxUsers int, log *slog.Logger) *identityObserver {
	if flush <= 0 {
		flush = 10 * time.Second
	}
	if seenCap <= 0 {
		seenCap = 100_000
	}
	if probConf <= 0 || probConf > 1 {
		probConf = 0.5
	}
	if fpMaxUsers <= 0 {
		fpMaxUsers = 5
	}
	return &identityObserver{
		writer:      writer,
		log:         log,
		ch:          make(chan observation, 4096),
		flush:       flush,
		seenCap:     seenCap,
		maxBatch:    500,
		probEnabled: probEnabled,
		probConf:    probConf,
		fpMaxUsers:  fpMaxUsers,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
}

func (o *identityObserver) Start() { go o.run() }

// Stop signals the batcher to flush and exit, and waits for it.
func (o *identityObserver) Stop() {
	if o == nil {
		return
	}
	close(o.stop)
	<-o.done
}

// idSource pairs an identifier value with its source/type.
type idSource struct {
	value  string
	source string
}

// Observe records the identity signals on this request. Cheap + non-blocking;
// safe on a nil receiver.
func (o *identityObserver) Observe(r *http.Request, userID, uid2 string) {
	if o == nil {
		return
	}
	ids := gatherIDs(r, userID, uid2)

	// Deterministic: every co-occurring pair is the same person.
	if len(ids) >= 2 {
		var edges []postgres.IdentityEdge
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				edges = append(edges, postgres.IdentityEdge{
					UserID:     ids[i].value,
					LinkedID:   ids[j].value,
					Source:     ids[j].source,
					LinkType:   identity.LinkObserved,
					Confidence: 1.0,
				})
			}
		}
		o.send(observation{edges: edges})
	}

	// Probabilistic: this user's id seen from this IP+UA fingerprint.
	if o.probEnabled && len(ids) >= 1 {
		if fp := requestFingerprint(r); fp != "" {
			o.send(observation{probID: ids[0].value, fp: fp})
		}
	}
}

func (o *identityObserver) send(obs observation) {
	select {
	case o.ch <- obs:
	default: // buffer full → drop; auto-build is best-effort
	}
}

// gatherIDs collects the distinct identifiers present on a request, in a fixed
// order so the pairs produced are deterministic. Duplicate values are dropped.
func gatherIDs(r *http.Request, userID, uid2 string) []idSource {
	q := r.URL.Query()
	candidates := []idSource{
		{userID, identity.SourcePublisherUserID},
		{uid2, identity.SourceUID2},
		{q.Get("hashed_email"), identity.SourceHashedEmail},
		{q.Get("ifa"), identity.SourceDeviceID},
		{q.Get("publisher_user_id"), identity.SourcePublisherUserID},
	}
	seen := make(map[string]struct{}, len(candidates))
	out := make([]idSource, 0, len(candidates))
	for _, c := range candidates {
		if c.value == "" {
			continue
		}
		if _, dup := seen[c.value]; dup {
			continue
		}
		seen[c.value] = struct{}{}
		out = append(out, c)
	}
	return out
}

// requestFingerprint is the end user's IP + user-agent, used for probabilistic
// device matching. Prefers explicit ?ip / ?ua (forwarded by the ad tag), then
// falls back to the X-Forwarded-For / User-Agent headers. Empty when either
// half is missing (we never fingerprint on IP alone — too coarse).
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

// edgeKey is an order-independent key including the source, so (a,b) and (b,a)
// dedupe together, but a deterministic edge and a probabilistic edge for the
// same pair stay distinct — a weak link seen first must not block the strong
// one (they're separate rows keyed by source in Postgres too).
func edgeKey(e postgres.IdentityEdge) string {
	a, b := e.UserID, e.LinkedID
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b + "\x00" + e.Source
}

func (o *identityObserver) run() {
	defer close(o.done)
	seen := make(map[string]struct{}, o.seenCap)
	fpBuckets := make(map[string][]string) // fingerprint -> recent primary ids
	var batch []postgres.IdentityEdge
	t := time.NewTicker(o.flush)
	defer t.Stop()

	take := func(e postgres.IdentityEdge) {
		k := edgeKey(e)
		if _, dup := seen[k]; dup {
			return
		}
		if len(seen) >= o.seenCap {
			// Bounded memory: reset. Re-writing an edge later is idempotent.
			seen = make(map[string]struct{}, o.seenCap)
		}
		seen[k] = struct{}{}
		batch = append(batch, e)
	}

	// probabilistic buckets a sighting and emits low-confidence edges to the
	// prior ids on the same fingerprint (unless it looks like a shared IP).
	probabilistic := func(id, fp string) {
		bucket := fpBuckets[fp]
		for _, existing := range bucket {
			if existing == id {
				return // already tracked on this fingerprint
			}
		}
		if len(bucket) >= o.fpMaxUsers {
			return // shared IP (too many distinct ids) → don't link
		}
		for _, other := range bucket {
			take(postgres.IdentityEdge{
				UserID:     id,
				LinkedID:   other,
				Source:     identity.SourceProbabilistic,
				LinkType:   identity.LinkCrossDevice,
				Confidence: o.probConf,
			})
		}
		if len(fpBuckets) >= o.seenCap {
			fpBuckets = make(map[string][]string) // bound fingerprint memory
			bucket = nil
		}
		fpBuckets[fp] = append(bucket, id)
	}

	apply := func(obs observation) {
		for _, e := range obs.edges {
			take(e)
		}
		if obs.fp != "" {
			probabilistic(obs.probID, obs.fp)
		}
		if len(batch) >= o.maxBatch {
			o.writeBatch(batch)
			batch = nil
		}
	}

	for {
		select {
		case <-o.stop:
			// Drain buffered observations, then flush, for a clean shutdown.
			for {
				select {
				case obs := <-o.ch:
					apply(obs)
				default:
					o.writeBatch(batch)
					return
				}
			}
		case obs := <-o.ch:
			apply(obs)
		case <-t.C:
			if len(batch) > 0 {
				o.writeBatch(batch)
				batch = nil
			}
		}
	}
}

func (o *identityObserver) writeBatch(batch []postgres.IdentityEdge) {
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, err := o.writer.LinkIdentity(ctx, batch)
	if err != nil {
		o.log.Error("identity observer flush failed (edges dropped)", "edges", len(batch), "error", err)
		return
	}
	o.log.Debug("identity edges written from observation", "count", n)
}

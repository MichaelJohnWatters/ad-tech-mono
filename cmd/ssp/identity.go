package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// identityWriter is the write dependency of the observer — satisfied by
// *postgres.Store (LinkIdentity).
type identityWriter interface {
	LinkIdentity(ctx context.Context, edges []postgres.IdentityEdge) (int, error)
}

// identityObserver auto-builds the identity graph from inbound requests: when a
// request carries two or more identifiers for the same user (user_id, uid2,
// hashed_email, device ifa, …) those are deterministically the same person, so
// we record edges linking them.
//
// It stays off the hot path: Observe just enqueues onto a buffered channel
// (dropping if full), and a background goroutine dedupes against a bounded
// recently-seen set and flushes new edges to Postgres in batches on an
// interval. A nil *identityObserver is a no-op, so callers needn't branch.
type identityObserver struct {
	writer   identityWriter
	log      *slog.Logger
	ch       chan postgres.IdentityEdge
	flush    time.Duration
	seenCap  int
	maxBatch int
	stop     chan struct{}
	done     chan struct{}
}

func newIdentityObserver(writer identityWriter, flush time.Duration, seenCap int, log *slog.Logger) *identityObserver {
	if flush <= 0 {
		flush = 10 * time.Second
	}
	if seenCap <= 0 {
		seenCap = 100_000
	}
	return &identityObserver{
		writer:   writer,
		log:      log,
		ch:       make(chan postgres.IdentityEdge, 4096),
		flush:    flush,
		seenCap:  seenCap,
		maxBatch: 500,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
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

// Observe extracts the identifiers on this request and enqueues edges linking
// each co-occurring pair. Cheap + non-blocking; safe on a nil receiver.
func (o *identityObserver) Observe(r *http.Request, userID, uid2 string) {
	if o == nil {
		return
	}
	ids := gatherIDs(r, userID, uid2)
	if len(ids) < 2 {
		return
	}
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			e := postgres.IdentityEdge{
				UserID:     ids[i].value,
				LinkedID:   ids[j].value,
				Source:     ids[j].source,
				LinkType:   identity.LinkObserved,
				Confidence: 1.0,
			}
			select {
			case o.ch <- e:
			default: // buffer full → drop; auto-build is best-effort
			}
		}
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

// edgeKey is an order-independent key for an edge, so (a,b) and (b,a) dedupe to
// one entry regardless of which id was seen first.
func edgeKey(e postgres.IdentityEdge) string {
	a, b := e.UserID, e.LinkedID
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b
}

func (o *identityObserver) run() {
	defer close(o.done)
	seen := make(map[string]struct{}, o.seenCap)
	var batch []postgres.IdentityEdge
	t := time.NewTicker(o.flush)
	defer t.Stop()

	// take applies dedup + accumulation for one edge, returning the (possibly
	// grown) batch.
	take := func(batch []postgres.IdentityEdge, e postgres.IdentityEdge) []postgres.IdentityEdge {
		k := edgeKey(e)
		if _, dup := seen[k]; dup {
			return batch
		}
		if len(seen) >= o.seenCap {
			// Bounded memory: reset the dedup set. Re-writing an edge later is
			// idempotent (LinkIdentity upserts), so this is safe.
			seen = make(map[string]struct{}, o.seenCap)
		}
		seen[k] = struct{}{}
		return append(batch, e)
	}

	for {
		select {
		case <-o.stop:
			// Drain anything still buffered, then flush, so a graceful shutdown
			// doesn't lose observed edges.
			for {
				select {
				case e := <-o.ch:
					batch = take(batch, e)
				default:
					o.writeBatch(batch)
					return
				}
			}
		case e := <-o.ch:
			batch = take(batch, e)
			if len(batch) >= o.maxBatch {
				o.writeBatch(batch)
				batch = nil
			}
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

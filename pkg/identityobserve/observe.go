package identityobserve

import (
	"context"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// Writer is the persistence dependency — satisfied by *postgres.Store.
type Writer interface {
	LinkIdentity(ctx context.Context, edges []postgres.IdentityEdge) (int, error)
}

// Config tunes the Observer.
type Config struct {
	Flush       time.Duration // batch flush interval (default 10s)
	SeenCap     int           // dedup/fingerprint set cap (default 100k)
	ProbEnabled bool          // enable probabilistic IP+UA matching
	ProbConf    float64       // confidence for probabilistic edges (default 0.5)
	FPMaxUsers  int           // shared-IP cap: skip fingerprints with more ids (default 5)
}

// Observer batches observed identity signals into graph edges and writes them.
//
//   - Deterministic: 2+ identifiers on one request are the same person (conf 1.0).
//   - Probabilistic (opt-in): different users seen from the same IP+UA are
//     likely the same device (conf <1.0), skipping shared IPs.
//
// Observe() enqueues onto a buffered channel (dropping if full); a single
// background goroutine owns all state (dedup set + fingerprint buckets, so no
// locking) and flushes to the Writer in batches.
type Observer struct {
	writer      Writer
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

type observation struct {
	edges  []postgres.IdentityEdge
	probID string
	fp     string
}

// New builds an Observer. Call Start before Observe.
func New(writer Writer, cfg Config, log *slog.Logger) *Observer {
	if cfg.Flush <= 0 {
		cfg.Flush = 10 * time.Second
	}
	if cfg.SeenCap <= 0 {
		cfg.SeenCap = 100_000
	}
	if cfg.ProbConf <= 0 || cfg.ProbConf > 1 {
		cfg.ProbConf = 0.5
	}
	if cfg.FPMaxUsers <= 0 {
		cfg.FPMaxUsers = 5
	}
	return &Observer{
		writer:      writer,
		log:         log,
		ch:          make(chan observation, 4096),
		flush:       cfg.Flush,
		seenCap:     cfg.SeenCap,
		maxBatch:    500,
		probEnabled: cfg.ProbEnabled,
		probConf:    cfg.ProbConf,
		fpMaxUsers:  cfg.FPMaxUsers,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
}

func (o *Observer) Start() { go o.run() }

// Stop flushes and exits, waiting for the batcher. Safe on nil.
func (o *Observer) Stop() {
	if o == nil {
		return
	}
	close(o.stop)
	<-o.done
}

// Observe records the identifiers (and optional IP+UA fingerprint) seen
// together on one request. Cheap + non-blocking; safe on nil.
func (o *Observer) Observe(ids []Signal, fingerprint string) {
	if o == nil {
		return
	}
	if len(ids) >= 2 {
		var edges []postgres.IdentityEdge
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				edges = append(edges, postgres.IdentityEdge{
					UserID:     ids[i].Value,
					LinkedID:   ids[j].Value,
					Source:     ids[j].Source,
					LinkType:   identity.LinkObserved,
					Confidence: 1.0,
				})
			}
		}
		o.send(observation{edges: edges})
	}
	if o.probEnabled && len(ids) >= 1 && fingerprint != "" {
		o.send(observation{probID: ids[0].Value, fp: fingerprint})
	}
}

func (o *Observer) send(obs observation) {
	select {
	case o.ch <- obs:
	default: // buffer full → drop; auto-build is best-effort
	}
}

// edgeKey is an order-independent key including the source, so (a,b) and (b,a)
// dedupe together but a deterministic and a probabilistic edge for the same
// pair stay distinct (a weak link seen first must not block the strong one).
func edgeKey(e postgres.IdentityEdge) string {
	a, b := e.UserID, e.LinkedID
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b + "\x00" + e.Source
}

func (o *Observer) run() {
	defer close(o.done)
	seen := make(map[string]struct{}, o.seenCap)
	fpBuckets := make(map[string][]string)
	var batch []postgres.IdentityEdge
	t := time.NewTicker(o.flush)
	defer t.Stop()

	take := func(e postgres.IdentityEdge) {
		k := edgeKey(e)
		if _, dup := seen[k]; dup {
			return
		}
		if len(seen) >= o.seenCap {
			seen = make(map[string]struct{}, o.seenCap) // bounded; re-writes are idempotent
		}
		seen[k] = struct{}{}
		batch = append(batch, e)
	}

	probabilistic := func(id, fp string) {
		bucket := fpBuckets[fp]
		for _, existing := range bucket {
			if existing == id {
				return
			}
		}
		if len(bucket) >= o.fpMaxUsers {
			return // shared IP → don't link
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
			fpBuckets = make(map[string][]string)
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

func (o *Observer) writeBatch(batch []postgres.IdentityEdge) {
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

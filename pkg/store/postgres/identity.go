package postgres

import (
	"context"
	"fmt"
)

// Identity graph access. The identity_graph table links a user_id to a
// linked_id (a UID2 token, hashed email, device id, another publisher's user
// id, …) so a user known by one identifier resolves to the others. The table
// is platform-global (no account_id / RLS) — links cross tenants by design.

// IdentityEdge is one link to write into the identity graph.
type IdentityEdge struct {
	UserID     string
	LinkedID   string
	Source     string  // identity.Source* (uid2, hashed_email, …)
	LinkType   string  // identity.Link* (cross_device, crm_match, …)
	Confidence float64 // 0..1; deterministic = 1.0
}

// LinkIdentity upserts a batch of edges. Idempotent on (user_id, linked_id,
// source): a re-link refreshes confidence + created_at. Self-links (user_id ==
// linked_id) and edges missing an id are skipped. Returns the number written.
func (s *Store) LinkIdentity(ctx context.Context, edges []IdentityEdge) (int, error) {
	const q = `
INSERT INTO identity_graph (user_id, linked_id, source, link_type, confidence, created_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (user_id, linked_id, source) DO UPDATE SET
    confidence = EXCLUDED.confidence,
    link_type  = EXCLUDED.link_type,
    created_at = now()`
	tx, err := s.primary.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("identity link begin: %w", err)
	}
	n := 0
	for _, e := range edges {
		if e.UserID == "" || e.LinkedID == "" || e.UserID == e.LinkedID {
			continue
		}
		conf := e.Confidence
		if conf <= 0 || conf > 1 {
			conf = 1.0
		}
		if _, err := tx.ExecContext(ctx, q, e.UserID, e.LinkedID, e.Source, e.LinkType, conf); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("identity link insert: %w", err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("identity link commit: %w", err)
	}
	return n, nil
}

// IdentityLink is one neighbour in the preloaded adjacency: a linked id and the
// confidence of that link. Callers can gate traversal on confidence.
type IdentityLink struct {
	ID         string
	Confidence float64
}

// LoadIdentityGraph loads the whole graph as a bidirectional adjacency map
// (id -> its linked ids + confidences), for callers that preload it into memory
// and resolve without touching Postgres on the hot path. Unexpired edges only.
//
// Id strings are interned so each unique identifier's bytes are stored once and
// shared across the map key and every slice entry it appears in — an id in the
// adjacency N times costs one copy, not N. Roughly halves the retained
// footprint versus one allocation per occurrence.
func (s *Store) LoadIdentityGraph(ctx context.Context) (map[string][]IdentityLink, error) {
	// confidence::float8 so lib/pq scans the NUMERIC column straight into a
	// float64 rather than a string.
	const q = `SELECT user_id, linked_id, confidence::float8 FROM identity_graph
		WHERE expires_at IS NULL OR expires_at > now()`
	rows, err := s.read.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("load identity graph: %w", err)
	}
	defer rows.Close()
	b := newAdjacencyBuilder()
	for rows.Next() {
		var u, l string
		var conf float64
		if err := rows.Scan(&u, &l, &conf); err != nil {
			return nil, fmt.Errorf("scan identity edge: %w", err)
		}
		b.add(u, l, conf)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return b.build(), nil
}

// adjacencyBuilder streams (user_id, linked_id, confidence) edges into a
// bidirectional adjacency map, interning id strings so each unique id's bytes
// are stored once and shared everywhere it appears. Kept as a type so it's
// unit-testable without a database.
type adjacencyBuilder struct {
	intern map[string]string
	sets   map[string]map[string]float64 // a -> (neighbour -> max confidence)
}

func newAdjacencyBuilder() *adjacencyBuilder {
	return &adjacencyBuilder{
		intern: make(map[string]string),
		sets:   make(map[string]map[string]float64),
	}
}

func (b *adjacencyBuilder) canon(v string) string {
	if c, ok := b.intern[v]; ok {
		return c
	}
	b.intern[v] = v
	return v
}

// add records the edge in both directions, skipping self/empty links.
func (b *adjacencyBuilder) add(u, l string, conf float64) {
	u, l = b.canon(u), b.canon(l) // share bytes across every occurrence
	b.link(u, l, conf)
	b.link(l, u, conf)
}

func (b *adjacencyBuilder) link(a, c string, conf float64) {
	if a == "" || c == "" || a == c {
		return
	}
	if b.sets[a] == nil {
		b.sets[a] = make(map[string]float64)
	}
	// Keep the strongest confidence when the same pair appears via multiple
	// edges (e.g. a deterministic and a probabilistic source).
	if cur, ok := b.sets[a][c]; !ok || conf > cur {
		b.sets[a][c] = conf
	}
}

func (b *adjacencyBuilder) build() map[string][]IdentityLink {
	adj := make(map[string][]IdentityLink, len(b.sets))
	for id, set := range b.sets {
		out := make([]IdentityLink, 0, len(set))
		for nid, conf := range set {
			out = append(out, IdentityLink{ID: nid, Confidence: conf})
		}
		adj[id] = out
	}
	return adj
}

// ResolveIdentity returns the distinct identifiers linked to id (in either
// direction), excluding id itself. Unexpired edges only. Used on the DSP bid
// path to expand a UID2 / user id to its linked ids for segment lookup.
func (s *Store) ResolveIdentity(ctx context.Context, id string) ([]string, error) {
	if id == "" {
		return nil, nil
	}
	const q = `
SELECT DISTINCT other FROM (
    SELECT linked_id AS other FROM identity_graph
      WHERE user_id = $1 AND (expires_at IS NULL OR expires_at > now())
    UNION
    SELECT user_id AS other FROM identity_graph
      WHERE linked_id = $1 AND (expires_at IS NULL OR expires_at > now())
) t
WHERE other <> $1
ORDER BY other`
	rows, err := s.read.QueryContext(ctx, q, id)
	if err != nil {
		return nil, fmt.Errorf("resolve identity %q: %w", id, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan linked id: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

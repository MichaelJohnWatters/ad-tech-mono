package identityobserve

import "strings"

// FPStore holds the probabilistic fingerprint→ids buckets. The default is an
// in-memory store (one consumer replica); a Redis-backed implementation (in
// cmd/identity-consumer) keeps the buckets coherent across multiple replicas.
type FPStore interface {
	// Observe records id on fingerprint fp and returns the OTHER ids already on
	// that fingerprint to link against — nil when id is already tracked or fp is
	// over capacity (a shared IP we won't link across).
	Observe(fp, id string, maxUsers int) []string
}

// memFPStore is the default in-memory FPStore. Only accessed from the observer's
// single run() goroutine, so it needs no locking.
type memFPStore struct {
	buckets map[string][]string
	cap     int // max fingerprints tracked before a reset (bounds memory)
}

func newMemFPStore(cap int) *memFPStore {
	if cap <= 0 {
		cap = 100_000
	}
	return &memFPStore{buckets: make(map[string][]string), cap: cap}
}

func (m *memFPStore) Observe(fp, id string, maxUsers int) []string {
	bucket := m.buckets[fp]
	for _, e := range bucket {
		if e == id {
			return nil
		}
	}
	if len(bucket) >= maxUsers {
		return nil // shared IP → don't link
	}
	others := append([]string(nil), bucket...) // snapshot before mutation
	if len(m.buckets) >= m.cap {
		m.buckets = make(map[string][]string) // bounded; edges are idempotent
		bucket = nil
	}
	m.buckets[fp] = append(bucket, id)
	return others
}

// NormalizeUA reduces a user-agent to a coarse family by stripping version
// numbers (digit and dot runs), so minor-version churn (Chrome/120 vs /121)
// doesn't split a device across fingerprints. Conservative fuzzy matching —
// the browser/OS structure survives, only the numbers go.
func NormalizeUA(ua string) string {
	var b strings.Builder
	b.Grow(len(ua))
	for _, r := range ua {
		if (r >= '0' && r <= '9') || r == '.' {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// normalizeFingerprint applies NormalizeUA to the UA half of an "ip|ua"
// fingerprint, leaving the IP exact.
func normalizeFingerprint(fp string) string {
	i := strings.IndexByte(fp, '|')
	if i < 0 {
		return fp
	}
	return fp[:i] + "|" + NormalizeUA(fp[i+1:])
}

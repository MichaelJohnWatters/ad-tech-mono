// Package identity provides the identity graph for cross-publisher,
// cross-device user linking.
//
// Matching rules:
//   - Deterministic: same hashed email = confirmed same person
//   - Probabilistic: same IP + similar UA = likely same person (lower confidence)
//   - Cross-device: same hashed email on different devices = same person
//
// Usage:
//
//	graph := identity.NewGraph()
//	graph.Link("platform-id-1", identity.Signal{Type: "hashed_email", Value: "sha256_abc"})
//	graph.Link("platform-id-2", identity.Signal{Type: "hashed_email", Value: "sha256_abc"})
//	ids := graph.Resolve("platform-id-1") // returns ["platform-id-1", "platform-id-2"]
package identity

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

// Signal is a piece of identifying information linked to a platform ID.
type Signal struct {
	Type       string // hashed_email, publisher_user_id, device_id, ip_ua
	Value      string
	Source     string // publisher_id or advertiser_id
	Confidence float64 // 0.0-1.0 (deterministic=1.0, probabilistic<1.0)
	Timestamp  time.Time
}

// Edge connects two platform IDs in the identity graph.
type Edge struct {
	PlatformID1 string
	PlatformID2 string
	Signal      Signal
	CreatedAt   time.Time
}

// UserProfile aggregates all known signals for a platform ID.
type UserProfile struct {
	PlatformID string
	Signals    []Signal
	LinkedIDs  []string // other platform IDs for the same person
	Segments   []string // audience segment IDs
	FirstSeen  time.Time
	LastSeen   time.Time
}

// Graph is the in-memory identity graph.
// In production, edges are stored in Postgres with Redis for fast lookups.
type Graph struct {
	mu       sync.RWMutex
	signals  map[string][]Signal            // platform_id -> signals
	index    map[string]map[string]bool     // signal_key -> set of platform_ids
	profiles map[string]*UserProfile        // platform_id -> profile
}

// NewGraph creates an empty identity graph.
func NewGraph() *Graph {
	return &Graph{
		signals:  make(map[string][]Signal),
		index:    make(map[string]map[string]bool),
		profiles: make(map[string]*UserProfile),
	}
}

// GeneratePlatformID creates a new unique platform ID.
func GeneratePlatformID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("pid-%x-%x-%x", b[0:4], b[4:6], b[6:8])
}

// Link associates a signal with a platform ID.
// If another platform ID already has the same deterministic signal,
// they are linked as the same person.
func (g *Graph) Link(platformID string, signal Signal) []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	if signal.Timestamp.IsZero() {
		signal.Timestamp = time.Now()
	}

	// Store signal
	g.signals[platformID] = append(g.signals[platformID], signal)

	// Update profile
	profile, ok := g.profiles[platformID]
	if !ok {
		profile = &UserProfile{
			PlatformID: platformID,
			FirstSeen:  signal.Timestamp,
		}
		g.profiles[platformID] = profile
	}
	profile.Signals = append(profile.Signals, signal)
	profile.LastSeen = signal.Timestamp

	// Index by signal key for cross-ID matching
	key := signalKey(signal)
	if g.index[key] == nil {
		g.index[key] = make(map[string]bool)
	}
	g.index[key][platformID] = true

	// Find linked IDs (other platform IDs with the same signal)
	var linked []string
	for pid := range g.index[key] {
		if pid != platformID {
			linked = append(linked, pid)
		}
	}

	// Update linked IDs on all connected profiles
	if len(linked) > 0 {
		allIDs := append(linked, platformID)
		for _, pid := range allIDs {
			if p, ok := g.profiles[pid]; ok {
				p.LinkedIDs = dedup(allIDs, pid)
			}
		}
	}

	return linked
}

// Resolve returns all platform IDs that belong to the same person.
func (g *Graph) Resolve(platformID string) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	result := map[string]bool{platformID: true}

	signals := g.signals[platformID]
	for _, s := range signals {
		key := signalKey(s)
		for pid := range g.index[key] {
			result[pid] = true
		}
	}

	var ids []string
	for id := range result {
		ids = append(ids, id)
	}
	return ids
}

// Profile returns the user profile for a platform ID.
func (g *Graph) Profile(platformID string) *UserProfile {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if p, ok := g.profiles[platformID]; ok {
		cp := *p
		return &cp
	}
	return nil
}

// Delete removes a platform ID and all its signals from the graph.
// Used for GDPR deletion requests.
func (g *Graph) Delete(platformID string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Remove from signal index
	for _, s := range g.signals[platformID] {
		key := signalKey(s)
		if ids, ok := g.index[key]; ok {
			delete(ids, platformID)
			if len(ids) == 0 {
				delete(g.index, key)
			}
		}
	}

	// Remove signals and profile
	delete(g.signals, platformID)
	delete(g.profiles, platformID)

	// Remove from linked profiles
	for _, p := range g.profiles {
		var kept []string
		for _, lid := range p.LinkedIDs {
			if lid != platformID {
				kept = append(kept, lid)
			}
		}
		p.LinkedIDs = kept
	}
}

// Stats returns graph statistics.
func (g *Graph) Stats() GraphStats {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return GraphStats{
		TotalProfiles:  len(g.profiles),
		TotalSignals:   len(g.index),
		TotalEdges:     countEdges(g.index),
	}
}

type GraphStats struct {
	TotalProfiles int
	TotalSignals  int
	TotalEdges    int
}

func signalKey(s Signal) string {
	return s.Type + ":" + s.Value
}

func dedup(ids []string, exclude string) []string {
	var result []string
	for _, id := range ids {
		if id != exclude {
			result = append(result, id)
		}
	}
	return result
}

func countEdges(index map[string]map[string]bool) int {
	total := 0
	for _, ids := range index {
		n := len(ids)
		total += n * (n - 1) / 2 // combinations
	}
	return total
}

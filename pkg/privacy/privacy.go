// Package privacy handles consent checking, the 3-level opt-out system,
// and data deletion propagation.
//
// Levels:
//   - Level 1: No personalisation (contextual ads only, keep data)
//   - Level 2: No tracking (delete cookie, identity graph, anonymise events)
//   - Level 3: Full deletion (purge everything, irreversible)
//
// Usage:
//
//	mgr := privacy.NewManager(identityGraph, audienceStore, logger)
//	mgr.OptOut(ctx, "user-123", privacy.Level2)
//	mgr.RequestDeletion(ctx, "user-123")
//	allowed := mgr.CheckConsent(ctx, "user-123", privacy.PurposeTargeting)
package privacy

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
)

// OptOutLevel represents the level of user opt-out.
type OptOutLevel int

const (
	LevelNone              OptOutLevel = 0
	LevelNoPersonalisation OptOutLevel = 1
	LevelNoTracking        OptOutLevel = 2
	LevelFullDeletion      OptOutLevel = 3
)

// Purpose represents a data processing purpose for consent checking.
type Purpose string

const (
	PurposeTargeting    Purpose = "targeting"
	PurposeFrequencyCap Purpose = "frequency_cap"
	PurposeMeasurement  Purpose = "measurement"
	PurposeRetargeting  Purpose = "retargeting"
	PurposeCrossDevice  Purpose = "cross_device"
)

// ConsentStatus represents a user's consent state.
type ConsentStatus struct {
	UserID    string
	Level     OptOutLevel
	Purposes  map[Purpose]bool // which purposes are consented
	Source    string           // cmp, platform, device
	Timestamp time.Time
}

// OptOutRecord is stored in the opt-out registry.
type OptOutRecord struct {
	UserID     string
	Level      OptOutLevel
	Source     string
	Timestamp  time.Time
	VerifiedAt *time.Time      // for Level 3 deletion
	Systems    map[string]bool // which systems have been purged
}

// DeletionResult tracks the outcome of a deletion request.
type DeletionResult struct {
	UserID          string
	Level           OptOutLevel
	SystemsPurged   []string
	SegmentsRemoved int
	Timestamp       time.Time
}

// Manager handles privacy operations.
type Manager struct {
	mu        sync.RWMutex
	registry  map[string]*OptOutRecord
	graph     *identity.Graph
	audiences *audience.Store
	log       *slog.Logger
}

// NewManager creates a privacy manager.
func NewManager(graph *identity.Graph, audiences *audience.Store, log *slog.Logger) *Manager {
	return &Manager{
		registry:  make(map[string]*OptOutRecord),
		graph:     graph,
		audiences: audiences,
		log:       log,
	}
}

// CheckConsent returns whether a purpose is allowed for a user.
func (m *Manager) CheckConsent(ctx context.Context, userID string, purpose Purpose) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	record, ok := m.registry[userID]
	if !ok {
		return true // no opt-out = full consent (default)
	}

	switch record.Level {
	case LevelNoPersonalisation:
		// Only contextual purposes allowed
		return purpose == PurposeMeasurement
	case LevelNoTracking, LevelFullDeletion:
		return false // nothing allowed
	default:
		return true
	}
}

// GetOptOutLevel returns the current opt-out level for a user.
func (m *Manager) GetOptOutLevel(userID string) OptOutLevel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if record, ok := m.registry[userID]; ok {
		return record.Level
	}
	return LevelNone
}

// OptOut processes an opt-out request at the given level.
func (m *Manager) OptOut(ctx context.Context, userID string, level OptOutLevel, source string) *DeletionResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	result := &DeletionResult{
		UserID:    userID,
		Level:     level,
		Timestamp: now,
	}

	// Store in registry
	m.registry[userID] = &OptOutRecord{
		UserID:    userID,
		Level:     level,
		Source:    source,
		Timestamp: now,
		Systems:   make(map[string]bool),
	}

	// Level 1: Remove from behavioural segments only
	if level >= LevelNoPersonalisation {
		removed := m.audiences.RemoveUserFromAll(ctx, userID)
		result.SegmentsRemoved = removed
		result.SystemsPurged = append(result.SystemsPurged, "audience_segments")
		m.registry[userID].Systems["audience_segments"] = true
	}

	// Level 2: Also delete identity graph
	if level >= LevelNoTracking {
		m.graph.Delete(userID)
		result.SystemsPurged = append(result.SystemsPurged, "identity_graph")
		m.registry[userID].Systems["identity_graph"] = true
		// In production: also delete Redis frequency cap keys, cookie
	}

	// Level 3: Queue full deletion (async job handles the rest)
	if level >= LevelFullDeletion {
		result.SystemsPurged = append(result.SystemsPurged, "deletion_queued")
		m.registry[userID].Systems["deletion_queued"] = true
		// In production: publish to NATS adtech.privacy.deletion_requested
	}

	m.log.Info("opt-out processed",
		"user_id", userID,
		"level", level,
		"source", source,
		"segments_removed", result.SegmentsRemoved,
		"systems", result.SystemsPurged,
	)

	return result
}

// VerifyDeletion checks that a Level 3 deletion was completed across all systems.
func (m *Manager) VerifyDeletion(ctx context.Context, userID string) *VerificationResult {
	m.mu.RLock()
	defer m.mu.RUnlock()

	record, ok := m.registry[userID]
	if !ok {
		return &VerificationResult{UserID: userID, Status: "not_found"}
	}
	if record.Level != LevelFullDeletion {
		return &VerificationResult{UserID: userID, Status: "not_deletion_request"}
	}

	// Check each system
	vr := &VerificationResult{
		UserID: userID,
		Status: "verified",
	}

	// Identity graph check
	profile := m.graph.Profile(userID)
	if profile != nil {
		vr.Status = "incomplete"
		vr.FailedSystems = append(vr.FailedSystems, "identity_graph")
	} else {
		vr.CompletedSystems = append(vr.CompletedSystems, "identity_graph")
	}

	// Audience check
	segs := m.audiences.SegmentsForUser(ctx, userID, "") // empty account = check all
	if len(segs) > 0 {
		vr.Status = "incomplete"
		vr.FailedSystems = append(vr.FailedSystems, "audience_segments")
	} else {
		vr.CompletedSystems = append(vr.CompletedSystems, "audience_segments")
	}

	return vr
}

// ListOptOuts returns all opt-out records (for compliance reporting).
func (m *Manager) ListOptOuts() []OptOutRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []OptOutRecord
	for _, r := range m.registry {
		result = append(result, *r)
	}
	return result
}

// Stats returns opt-out statistics.
func (m *Manager) Stats() PrivacyStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var s PrivacyStats
	for _, r := range m.registry {
		s.Total++
		switch r.Level {
		case LevelNoPersonalisation:
			s.Level1++
		case LevelNoTracking:
			s.Level2++
		case LevelFullDeletion:
			s.Level3++
		}
	}
	return s
}

// VerificationResult is the outcome of verifying a deletion.
type VerificationResult struct {
	UserID           string
	Status           string // verified, incomplete, not_found
	CompletedSystems []string
	FailedSystems    []string
}

// PrivacyStats holds aggregate opt-out stats.
type PrivacyStats struct {
	Total  int
	Level1 int
	Level2 int
	Level3 int
}

// Package audience provides audience segment management.
//
// Segment types:
//   - First-party: advertiser CRM uploads (hashed emails)
//   - Behavioural: built from event data (visited page, clicked ad)
//   - Lookalike: modelled from a seed segment
//   - Composite: boolean combinations of other segments (A AND B, A OR NOT C)
//   - Suppression: exclusion lists (existing customers for acquisition campaigns)
//
// Access control: each segment has an owner (account_id).
// Advertisers can only target their own segments + publisher-shared segments.
//
// Usage:
//
//	store := audience.NewStore()
//	store.CreateSegment(ctx, segment)
//	store.AddUsers(ctx, segmentID, userIDs)
//	segments := store.SegmentsForUser(ctx, userID, accessAccountID)
package audience

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// SegmentType classifies how a segment was created.
type SegmentType string

const (
	SegmentFirstParty  SegmentType = "first_party"
	SegmentBehavioural SegmentType = "behavioural"
	SegmentLookalike   SegmentType = "lookalike"
	SegmentComposite   SegmentType = "composite"
	SegmentSuppression SegmentType = "suppression"
)

// Segment is an audience segment definition.
type Segment struct {
	ID          string
	AccountID   string // owner
	Name        string
	Type        SegmentType
	Description string
	Size        int64 // estimated user count
	Status      string // active, archived
	Shared      bool   // visible to other accounts for targeting
	Suppression bool   // use as exclusion list
	// Composite fields
	CompositeRule *CompositeRule
	// Lookalike fields
	SeedSegmentID string
	LookalikeSize int64 // target audience size
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// CompositeRule defines a boolean combination of segments.
type CompositeRule struct {
	Operator string   // "and", "or", "and_not"
	Segments []string // segment IDs
}

// Store manages audience segments and membership.
type Store struct {
	mu       sync.RWMutex
	segments map[string]*Segment
	members  map[string]map[string]bool // segment_id -> set of user_ids
	userSegs map[string]map[string]bool // user_id -> set of segment_ids
}

// NewStore creates an in-memory audience store.
func NewStore() *Store {
	return &Store{
		segments: make(map[string]*Segment),
		members:  make(map[string]map[string]bool),
		userSegs: make(map[string]map[string]bool),
	}
}

// CreateSegment adds a new segment.
func (s *Store) CreateSegment(_ context.Context, seg Segment) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if seg.ID == "" {
		return fmt.Errorf("segment ID required")
	}
	if seg.CreatedAt.IsZero() {
		seg.CreatedAt = time.Now()
	}
	seg.UpdatedAt = seg.CreatedAt
	seg.Status = "active"

	s.segments[seg.ID] = &seg
	s.members[seg.ID] = make(map[string]bool)
	return nil
}

// GetSegment returns a segment by ID.
func (s *Store) GetSegment(_ context.Context, segmentID string) *Segment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if seg, ok := s.segments[segmentID]; ok {
		cp := *seg
		cp.Size = int64(len(s.members[segmentID]))
		return &cp
	}
	return nil
}

// ListSegments returns all segments visible to the given account.
func (s *Store) ListSegments(_ context.Context, accountID string) []Segment {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []Segment
	for _, seg := range s.segments {
		if seg.AccountID == accountID || seg.Shared {
			cp := *seg
			cp.Size = int64(len(s.members[seg.ID]))
			result = append(result, cp)
		}
	}
	return result
}

// AddUsers adds users to a segment.
func (s *Store) AddUsers(_ context.Context, segmentID string, userIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	members, ok := s.members[segmentID]
	if !ok {
		return fmt.Errorf("segment %s not found", segmentID)
	}

	for _, uid := range userIDs {
		members[uid] = true
		if s.userSegs[uid] == nil {
			s.userSegs[uid] = make(map[string]bool)
		}
		s.userSegs[uid][segmentID] = true
	}

	if seg, ok := s.segments[segmentID]; ok {
		seg.Size = int64(len(members))
		seg.UpdatedAt = time.Now()
	}
	return nil
}

// RemoveUser removes a user from a segment.
func (s *Store) RemoveUser(_ context.Context, segmentID, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if members, ok := s.members[segmentID]; ok {
		delete(members, userID)
	}
	if segs, ok := s.userSegs[userID]; ok {
		delete(segs, segmentID)
	}
}

// RemoveUserFromAll removes a user from all segments (for deletion/opt-out).
func (s *Store) RemoveUserFromAll(_ context.Context, userID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	segs := s.userSegs[userID]
	removed := len(segs)
	for segID := range segs {
		if members, ok := s.members[segID]; ok {
			delete(members, userID)
		}
	}
	delete(s.userSegs, userID)
	return removed
}

// SegmentsForUser returns all segments a user belongs to,
// filtered by access control (only segments owned by or shared with accessAccountID).
func (s *Store) SegmentsForUser(_ context.Context, userID, accessAccountID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []string
	for segID := range s.userSegs[userID] {
		seg, ok := s.segments[segID]
		if !ok {
			continue
		}
		if seg.AccountID == accessAccountID || seg.Shared {
			result = append(result, segID)
		}
	}
	return result
}

// IsInSegment checks if a user is in a specific segment.
func (s *Store) IsInSegment(_ context.Context, segmentID, userID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if members, ok := s.members[segmentID]; ok {
		return members[userID]
	}
	return false
}

// EvaluateComposite evaluates a composite segment for a user.
func (s *Store) EvaluateComposite(_ context.Context, rule CompositeRule, userID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	switch rule.Operator {
	case "and":
		for _, segID := range rule.Segments {
			if members, ok := s.members[segID]; !ok || !members[userID] {
				return false
			}
		}
		return true
	case "or":
		for _, segID := range rule.Segments {
			if members, ok := s.members[segID]; ok && members[userID] {
				return true
			}
		}
		return false
	case "and_not":
		// First segment must match, rest must NOT match
		if len(rule.Segments) == 0 {
			return false
		}
		if members, ok := s.members[rule.Segments[0]]; !ok || !members[userID] {
			return false
		}
		for _, segID := range rule.Segments[1:] {
			if members, ok := s.members[segID]; ok && members[userID] {
				return false
			}
		}
		return true
	default:
		return false
	}
}

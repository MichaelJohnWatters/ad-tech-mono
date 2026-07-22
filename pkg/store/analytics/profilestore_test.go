package analytics

import (
	"context"
	"testing"
	"time"
)

// ADR 0006 phase 1: behaviour_signals + profile_signals land in the analytics
// store via BatchInserter alongside the existing lake dual-write. These test the
// memory backend (the CI/test default) — bulk append and, for profile signals,
// the one-row-per-id shape produced by the reporting handler's expansion.

func TestMemoryStore_InsertBehaviourSignals(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	now := time.Now().UTC()

	rows := []*BehaviourSignalRow{
		{TraceID: "t1", Kind: "site_visit", UserID: "u1", AccountID: "acct1", Tag: "product-page", ObservedAt: now},
		{TraceID: "t2", Kind: "impression", UserID: "u1", CampaignID: "c1", ObservedAt: now},
		nil, // nil rows are skipped, not panicked on
	}
	if err := s.InsertBehaviourSignals(ctx, rows); err != nil {
		t.Fatalf("InsertBehaviourSignals: %v", err)
	}
	got := s.BehaviourSignals()
	if len(got) != 2 {
		t.Fatalf("stored %d behaviour signals, want 2 (nil skipped)", len(got))
	}
	if got[0].Tag != "product-page" || got[0].AccountID != "acct1" {
		t.Errorf("site_visit row not preserved: %+v", got[0])
	}

	// Empty slice is a no-op (matches the other batch inserts).
	if err := s.InsertBehaviourSignals(ctx, nil); err != nil {
		t.Fatalf("empty InsertBehaviourSignals: %v", err)
	}
	if len(s.BehaviourSignals()) != 2 {
		t.Errorf("empty insert changed row count")
	}
}

func TestMemoryStore_InsertProfileSignals_ExpandedRows(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	now := time.Now().UTC()

	// One onboarding event expands upstream into 3 per-id rows; the store just
	// holds the expanded shape (the reporting handler does the expansion).
	rows := []*ProfileSignalRow{
		{TraceID: "tr", AccountID: "acct1", Source: "crm_upload", SegmentID: "seg1", SegmentName: "High Value", Consent: true, IDType: "hashed_email", IDValue: "h1", ObservedAt: now},
		{TraceID: "tr", AccountID: "acct1", Source: "crm_upload", SegmentID: "seg1", SegmentName: "High Value", Consent: true, IDType: "hashed_email", IDValue: "h2", ObservedAt: now},
		{TraceID: "tr", AccountID: "acct1", Source: "crm_upload", SegmentID: "seg1", SegmentName: "High Value", Consent: true, IDType: "uid2", IDValue: "u2", ObservedAt: now},
	}
	if err := s.InsertProfileSignals(ctx, rows); err != nil {
		t.Fatalf("InsertProfileSignals: %v", err)
	}
	got := s.ProfileSignals()
	if len(got) != 3 {
		t.Fatalf("stored %d profile signals, want 3 (one per id)", len(got))
	}
	// Each row carries the shared event fields plus its own id.
	byVal := map[string]string{}
	for _, r := range got {
		if r.SegmentID != "seg1" || r.AccountID != "acct1" || !r.Consent {
			t.Errorf("shared fields not preserved on expanded row: %+v", r)
		}
		byVal[r.IDValue] = r.IDType
	}
	if byVal["h1"] != "hashed_email" || byVal["u2"] != "uid2" {
		t.Errorf("per-id type/value mismatch: %v", byVal)
	}
}

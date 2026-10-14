package store

import (
	"context"
	"errors"
	"testing"
)

type serialOnly struct{ calls []string }

func (s *serialOnly) SegmentsForUser(context.Context, string) ([]string, error) { return nil, nil }
func (s *serialOnly) DSPSegmentsForUser(_ context.Context, id string) ([]string, error) {
	s.calls = append(s.calls, id)
	if id == "bad" {
		return nil, errors.New("redis blip")
	}
	return []string{"seg-" + id}, nil
}

// A Lookup without the batch capability still works through the helper:
// serial calls, index-aligned, and a failing id degrades to "no segments"
// instead of failing the whole bid's lookup.
func TestDSPSegmentsForUsers_SerialFallbackDegradesPerID(t *testing.T) {
	s := &serialOnly{}
	got, err := DSPSegmentsForUsers(context.Background(), s, []string{"a", "bad", "c"})
	if err != nil {
		t.Fatalf("fallback must not surface per-id errors: %v", err)
	}
	if len(s.calls) != 3 {
		t.Fatalf("want 3 serial calls, got %v", s.calls)
	}
	if len(got) != 3 || got[0][0] != "seg-a" || got[1] != nil || got[2][0] != "seg-c" {
		t.Fatalf("misaligned/undegraded result: %v", got)
	}
}

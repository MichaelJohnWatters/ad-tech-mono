package main

import (
	"context"
	"errors"
	"sort"
	"testing"
)

// fakeAud implements audstore.Lookup for the DSP private-segment path.
type fakeAud struct {
	byUser map[string][]string
	err    error
}

func (f *fakeAud) SegmentsForUser(_ context.Context, id string) ([]string, error) {
	return f.byUser[id], f.err
}
func (f *fakeAud) DSPSegmentsForUser(_ context.Context, id string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byUser[id], nil
}

type fakeResolver struct {
	linked map[string][]string
	err    error
}

func (f *fakeResolver) ResolveIdentity(_ context.Context, id string) ([]string, error) {
	return f.linked[id], f.err
}

func sortedEq(got, want []string) bool {
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestDSPPrivateSegments(t *testing.T) {
	aud := &fakeAud{byUser: map[string][]string{
		"user-1":  {"seg-a"},
		"email-1": {"seg-b", "seg-a"}, // seg-a overlaps → must dedupe
		"dev-1":   {"seg-c"},
	}}
	res := &fakeResolver{linked: map[string][]string{
		"user-1": {"email-1", "dev-1"},
	}}

	t.Run("no resolver → user segments only", func(t *testing.T) {
		got := dspPrivateSegments(context.Background(), aud, nil, "user-1", 10, quietMgmtLog())
		if !sortedEq(got, []string{"seg-a"}) {
			t.Errorf("got %v, want [seg-a]", got)
		}
	})

	t.Run("resolver unions + dedupes across linked ids", func(t *testing.T) {
		got := dspPrivateSegments(context.Background(), aud, res, "user-1", 10, quietMgmtLog())
		if !sortedEq(got, []string{"seg-a", "seg-b", "seg-c"}) {
			t.Errorf("got %v, want [seg-a seg-b seg-c]", got)
		}
	})

	t.Run("maxLinked caps linked expansion", func(t *testing.T) {
		// maxLinked 1 → userKey + at most 1 linked id (email-1); dev-1 dropped.
		got := dspPrivateSegments(context.Background(), aud, res, "user-1", 1, quietMgmtLog())
		if !sortedEq(got, []string{"seg-a", "seg-b"}) {
			t.Errorf("got %v, want [seg-a seg-b] (dev-1 beyond cap)", got)
		}
	})

	t.Run("resolver error degrades to user-only", func(t *testing.T) {
		bad := &fakeResolver{err: errors.New("db down")}
		got := dspPrivateSegments(context.Background(), aud, bad, "user-1", 10, quietMgmtLog())
		if !sortedEq(got, []string{"seg-a"}) {
			t.Errorf("got %v, want [seg-a] on resolver error", got)
		}
	})

	t.Run("nil store or empty key → nil", func(t *testing.T) {
		if got := dspPrivateSegments(context.Background(), nil, res, "user-1", 10, quietMgmtLog()); got != nil {
			t.Errorf("nil store: got %v, want nil", got)
		}
		if got := dspPrivateSegments(context.Background(), aud, res, "", 10, quietMgmtLog()); got != nil {
			t.Errorf("empty key: got %v, want nil", got)
		}
	})
}

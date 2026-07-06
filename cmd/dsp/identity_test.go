package main

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

func lk(id string, conf float64) postgres.IdentityLink {
	return postgres.IdentityLink{ID: id, Confidence: conf}
}

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

// fakeLoader stands in for postgres.Store.LoadIdentityGraph.
type fakeLoader struct {
	adj map[string][]postgres.IdentityLink
	err error
}

func (f *fakeLoader) LoadIdentityGraph(_ context.Context) (map[string][]postgres.IdentityLink, error) {
	return f.adj, f.err
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

func TestPreloadIdentityResolver(t *testing.T) {
	ctx := context.Background()
	ld := &fakeLoader{adj: map[string][]postgres.IdentityLink{"u": {lk("a", 1), lk("b", 1)}}}
	p := newPreloadIdentityResolver(ld, time.Hour, 1, 0, quietMgmtLog())

	t.Run("nil before first load", func(t *testing.T) {
		if got, _ := p.ResolveIdentity(ctx, "u"); got != nil {
			t.Errorf("got %v, want nil before any load", got)
		}
	})

	p.refresh() // synchronous — no goroutine/ticker in the test

	t.Run("serves the snapshot after load", func(t *testing.T) {
		if got, _ := p.ResolveIdentity(ctx, "u"); !sortedEq(got, []string{"a", "b"}) {
			t.Errorf("got %v, want [a b]", got)
		}
		if got, _ := p.ResolveIdentity(ctx, "unknown"); got != nil {
			t.Errorf("unknown id: got %v, want nil", got)
		}
	})

	t.Run("failed refresh keeps last-good snapshot", func(t *testing.T) {
		ld.adj = map[string][]postgres.IdentityLink{"u": {lk("c", 1)}}
		ld.err = errors.New("db down")
		p.refresh()
		if got, _ := p.ResolveIdentity(ctx, "u"); !sortedEq(got, []string{"a", "b"}) {
			t.Errorf("got %v, want stale [a b] preserved on error", got)
		}
	})

	t.Run("successful refresh swaps the snapshot", func(t *testing.T) {
		ld.err = nil // adj is now {"u":[c]}
		p.refresh()
		if got, _ := p.ResolveIdentity(ctx, "u"); !sortedEq(got, []string{"c"}) {
			t.Errorf("got %v, want refreshed [c]", got)
		}
	})
}

func TestPreloadIdentityResolver_Transitive(t *testing.T) {
	ctx := context.Background()
	// Bidirectional chain: u↔a (1.0), a↔b (1.0), b↔c (0.5).
	ld := &fakeLoader{adj: map[string][]postgres.IdentityLink{
		"u": {lk("a", 1.0)},
		"a": {lk("u", 1.0), lk("b", 1.0)},
		"b": {lk("a", 1.0), lk("c", 0.5)},
		"c": {lk("b", 0.5)},
	}}

	load := func(maxDepth int, minConf float64) *preloadIdentityResolver {
		p := newPreloadIdentityResolver(ld, time.Hour, maxDepth, minConf, quietMgmtLog())
		p.refresh()
		return p
	}

	t.Run("depth 1 = direct links only", func(t *testing.T) {
		got, _ := load(1, 0).ResolveIdentity(ctx, "u")
		if !sortedEq(got, []string{"a"}) {
			t.Errorf("got %v, want [a]", got)
		}
	})
	t.Run("depth 2 = linked-of-linked", func(t *testing.T) {
		got, _ := load(2, 0).ResolveIdentity(ctx, "u")
		if !sortedEq(got, []string{"a", "b"}) {
			t.Errorf("got %v, want [a b]", got)
		}
	})
	t.Run("depth 3 = full transitive closure", func(t *testing.T) {
		got, _ := load(3, 0).ResolveIdentity(ctx, "u")
		if !sortedEq(got, []string{"a", "b", "c"}) {
			t.Errorf("got %v, want [a b c]", got)
		}
	})
	t.Run("confidence gate stops at the weak edge", func(t *testing.T) {
		// depth 3 but min-confidence 0.6 excludes the b↔c edge (0.5).
		got, _ := load(3, 0.6).ResolveIdentity(ctx, "u")
		if !sortedEq(got, []string{"a", "b"}) {
			t.Errorf("got %v, want [a b] (c behind a 0.5 edge)", got)
		}
	})
}

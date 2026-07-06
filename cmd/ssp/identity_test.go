package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeWriter struct {
	mu    sync.Mutex
	edges []postgres.IdentityEdge
}

func (f *fakeWriter) LinkIdentity(_ context.Context, e []postgres.IdentityEdge) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edges = append(f.edges, e...)
	return len(e), nil
}

func (f *fakeWriter) pairs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.edges))
	for _, e := range f.edges {
		out = append(out, edgeKey(e))
	}
	sort.Strings(out)
	return out
}

func TestGatherIDs(t *testing.T) {
	r := httptest.NewRequest("GET", "/serve?hashed_email=E&ifa=D", nil)
	ids := gatherIDs(r, "USER", "U")
	// Expect user_id, uid2, hashed_email, ifa — in that fixed order.
	want := []string{"USER", "U", "E", "D"}
	if len(ids) != len(want) {
		t.Fatalf("got %d ids, want %d: %+v", len(ids), len(want), ids)
	}
	for i, w := range want {
		if ids[i].value != w {
			t.Errorf("ids[%d].value = %q, want %q", i, ids[i].value, w)
		}
	}

	t.Run("dedupes repeated values and skips empty", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/serve?hashed_email=SAME", nil)
		ids := gatherIDs(r, "SAME", "") // user_id == hashed_email value, uid2 empty
		if len(ids) != 1 {
			t.Errorf("got %d ids, want 1 (deduped): %+v", len(ids), ids)
		}
	})

	t.Run("fewer than 2 ids yields no pairs", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/serve", nil)
		if ids := gatherIDs(r, "solo", ""); len(ids) != 1 {
			t.Errorf("got %d, want 1", len(ids))
		}
	})
}

func TestEdgeKeyOrderIndependent(t *testing.T) {
	a := postgres.IdentityEdge{UserID: "x", LinkedID: "y"}
	b := postgres.IdentityEdge{UserID: "y", LinkedID: "x"}
	if edgeKey(a) != edgeKey(b) {
		t.Errorf("edgeKey not order-independent: %q vs %q", edgeKey(a), edgeKey(b))
	}
}

func TestIdentityObserver_WritesDedupedPairs(t *testing.T) {
	fw := &fakeWriter{}
	o := newIdentityObserver(fw, time.Hour, 1000, quietLog()) // long flush → Stop drives the flush
	o.Start()

	// A request with user_id + uid2 + hashed_email → 3 ids → 3 pairwise edges.
	r := httptest.NewRequest("GET", "/serve?hashed_email=E", nil)
	o.Observe(r, "USER", "U")
	o.Observe(r, "USER", "U") // identical → must dedupe
	o.Stop()                  // drains + flushes

	got := fw.pairs()
	want := []string{
		edgeKey(postgres.IdentityEdge{UserID: "USER", LinkedID: "U"}),
		edgeKey(postgres.IdentityEdge{UserID: "USER", LinkedID: "E"}),
		edgeKey(postgres.IdentityEdge{UserID: "U", LinkedID: "E"}),
	}
	sort.Strings(want)
	if len(got) != 3 {
		t.Fatalf("wrote %d edges, want 3 (deduped): %v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("edge[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Every written edge is a deterministic co-occurrence.
	for _, e := range fw.edges {
		if e.LinkType != identity.LinkObserved || e.Confidence != 1.0 {
			t.Errorf("edge %+v: want link_type=%s confidence=1.0", e, identity.LinkObserved)
		}
	}
}

func TestIdentityObserver_NilIsNoOp(t *testing.T) {
	var o *identityObserver
	// Must not panic on a nil receiver.
	o.Observe(httptest.NewRequest("GET", "/serve?hashed_email=E", nil), "USER", "U")
	o.Stop()
}

func TestIdentityObserver_SingleIDWritesNothing(t *testing.T) {
	fw := &fakeWriter{}
	o := newIdentityObserver(fw, time.Hour, 1000, quietLog())
	o.Start()
	o.Observe(httptest.NewRequest("GET", "/serve", nil), "solo", "") // one id only
	o.Stop()
	if len(fw.edges) != 0 {
		t.Errorf("wrote %d edges for a single-id request, want 0", len(fw.edges))
	}
}

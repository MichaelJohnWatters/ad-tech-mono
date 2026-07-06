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

// pairOnly is the sorted id pair, ignoring source — for asserting "which pairs
// were written" independent of the source tag.
func pairOnly(e postgres.IdentityEdge) string {
	a, b := e.UserID, e.LinkedID
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

func (f *fakeWriter) pairs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.edges))
	for _, e := range f.edges {
		out = append(out, pairOnly(e))
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
	// Deterministic only (probabilistic disabled). Long flush → Stop drives it.
	o := newIdentityObserver(fw, time.Hour, 1000, false, 0, 0, quietLog())
	o.Start()

	// A request with user_id + uid2 + hashed_email → 3 ids → 3 pairwise edges.
	r := httptest.NewRequest("GET", "/serve?hashed_email=E", nil)
	o.Observe(r, "USER", "U")
	o.Observe(r, "USER", "U") // identical → must dedupe
	o.Stop()                  // drains + flushes

	got := fw.pairs()
	want := []string{
		pairOnly(postgres.IdentityEdge{UserID: "USER", LinkedID: "U"}),
		pairOnly(postgres.IdentityEdge{UserID: "USER", LinkedID: "E"}),
		pairOnly(postgres.IdentityEdge{UserID: "U", LinkedID: "E"}),
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
	for _, e := range fw.edges {
		if e.LinkType != identity.LinkObserved || e.Confidence != 1.0 {
			t.Errorf("edge %+v: want link_type=%s confidence=1.0", e, identity.LinkObserved)
		}
	}
}

func TestIdentityObserver_Probabilistic(t *testing.T) {
	fw := &fakeWriter{}
	o := newIdentityObserver(fw, time.Hour, 1000, true, 0.5, 3, quietLog())
	o.Start()

	fp := "/serve?ip=1.2.3.4&ua=Mozilla" // same IP+UA across requests
	o.Observe(httptest.NewRequest("GET", fp, nil), "userA", "")
	o.Observe(httptest.NewRequest("GET", fp, nil), "userB", "") // → link A↔B
	o.Observe(httptest.NewRequest("GET", fp, nil), "userA", "") // repeat → no new link
	// A different fingerprint must not link to the first bucket.
	o.Observe(httptest.NewRequest("GET", "/serve?ip=9.9.9.9&ua=Mozilla", nil), "userC", "")
	o.Stop()

	if len(fw.edges) != 1 {
		t.Fatalf("wrote %d probabilistic edges, want 1: %+v", len(fw.edges), fw.edges)
	}
	e := fw.edges[0]
	if e.Confidence != 0.5 || e.Source != identity.SourceProbabilistic {
		t.Errorf("edge %+v: want confidence 0.5, source %s", e, identity.SourceProbabilistic)
	}
	if pairOnly(e) != pairOnly(postgres.IdentityEdge{UserID: "userA", LinkedID: "userB"}) {
		t.Errorf("wrong pair: %+v", e)
	}
}

func TestIdentityObserver_ProbabilisticSharedIPCap(t *testing.T) {
	fw := &fakeWriter{}
	// fpMaxUsers = 2: once a fingerprint has 2 distinct ids, a 3rd is treated as
	// a shared IP and not linked.
	o := newIdentityObserver(fw, time.Hour, 1000, true, 0.5, 2, quietLog())
	o.Start()
	fp := "/serve?ip=1.1.1.1&ua=UA"
	o.Observe(httptest.NewRequest("GET", fp, nil), "u1", "") // bucket [u1]
	o.Observe(httptest.NewRequest("GET", fp, nil), "u2", "") // link u1↔u2, bucket [u1,u2]
	o.Observe(httptest.NewRequest("GET", fp, nil), "u3", "") // bucket full → no link
	o.Stop()
	if len(fw.edges) != 1 {
		t.Errorf("wrote %d edges, want 1 (u3 skipped as shared IP): %+v", len(fw.edges), fw.edges)
	}
}

func TestIdentityObserver_NilIsNoOp(t *testing.T) {
	var o *identityObserver
	o.Observe(httptest.NewRequest("GET", "/serve?hashed_email=E", nil), "USER", "U")
	o.Stop()
}

func TestIdentityObserver_SingleIDWritesNothing(t *testing.T) {
	fw := &fakeWriter{}
	o := newIdentityObserver(fw, time.Hour, 1000, false, 0, 0, quietLog())
	o.Start()
	o.Observe(httptest.NewRequest("GET", "/serve", nil), "solo", "") // one id, no fingerprint
	o.Stop()
	if len(fw.edges) != 0 {
		t.Errorf("wrote %d edges for a single-id request, want 0", len(fw.edges))
	}
}

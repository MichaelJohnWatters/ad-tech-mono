package identityobserve

import (
	"context"
	"io"
	"log/slog"
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

func sig(v, s string) Signal { return Signal{Value: v, Source: s} }

func TestObserver_DeterministicDedupedPairs(t *testing.T) {
	fw := &fakeWriter{}
	o := New(fw, Config{Flush: time.Hour, SeenCap: 1000}, quietLog())
	o.Start()

	ids := []Signal{sig("USER", identity.SourcePublisherUserID), sig("U", identity.SourceUID2), sig("E", identity.SourceHashedEmail)}
	o.Observe(ids, "")
	o.Observe(ids, "") // dedupe
	o.Stop()

	got := fw.pairs()
	want := []string{"E|U", "U|USER", "E|USER"}
	sort.Strings(want)
	if len(got) != 3 {
		t.Fatalf("wrote %d edges, want 3: %v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pair[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, e := range fw.edges {
		if e.LinkType != identity.LinkObserved || e.Confidence != 1.0 {
			t.Errorf("edge %+v: want deterministic", e)
		}
	}
}

func TestObserver_Probabilistic(t *testing.T) {
	fw := &fakeWriter{}
	o := New(fw, Config{Flush: time.Hour, SeenCap: 1000, ProbEnabled: true, ProbConf: 0.5, FPMaxUsers: 3}, quietLog())
	o.Start()

	o.Observe([]Signal{sig("userA", identity.SourcePublisherUserID)}, "1.2.3.4|UA")
	o.Observe([]Signal{sig("userB", identity.SourcePublisherUserID)}, "1.2.3.4|UA") // link A↔B
	o.Observe([]Signal{sig("userA", identity.SourcePublisherUserID)}, "1.2.3.4|UA") // repeat, no new
	o.Observe([]Signal{sig("userC", identity.SourcePublisherUserID)}, "9.9.9.9|UA") // other fp
	o.Stop()

	if len(fw.edges) != 1 {
		t.Fatalf("wrote %d edges, want 1: %+v", len(fw.edges), fw.edges)
	}
	e := fw.edges[0]
	if e.Confidence != 0.5 || e.Source != identity.SourceProbabilistic {
		t.Errorf("edge %+v: want conf 0.5 source probabilistic", e)
	}
	if pairOnly(e) != "userA|userB" {
		t.Errorf("wrong pair: %+v", e)
	}
}

func TestObserver_SharedIPCap(t *testing.T) {
	fw := &fakeWriter{}
	o := New(fw, Config{Flush: time.Hour, ProbEnabled: true, ProbConf: 0.5, FPMaxUsers: 2}, quietLog())
	o.Start()
	fp := "1.1.1.1|UA"
	o.Observe([]Signal{sig("u1", "x")}, fp)
	o.Observe([]Signal{sig("u2", "x")}, fp) // link
	o.Observe([]Signal{sig("u3", "x")}, fp) // full → skip
	o.Stop()
	if len(fw.edges) != 1 {
		t.Errorf("wrote %d edges, want 1 (u3 skipped): %+v", len(fw.edges), fw.edges)
	}
}

func TestObserver_NilAndSingle(t *testing.T) {
	var o *Observer
	o.Observe([]Signal{sig("a", "x")}, "") // nil no-op, must not panic
	o.Stop()

	fw := &fakeWriter{}
	o2 := New(fw, Config{Flush: time.Hour}, quietLog())
	o2.Start()
	o2.Observe([]Signal{sig("solo", "x")}, "") // one id, no fp → nothing
	o2.Stop()
	if len(fw.edges) != 0 {
		t.Errorf("single id wrote %d edges, want 0", len(fw.edges))
	}
}

func TestEventRoundTrip(t *testing.T) {
	in := ObservedEvent{TraceID: "t1", IDs: []Signal{sig("a", "uid2"), sig("b", "hashed_email")}, Fingerprint: "ip|ua"}
	b, err := Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := Unmarshal(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.SchemaVersion != SchemaVersion {
		t.Errorf("schema version = %d, want %d", out.SchemaVersion, SchemaVersion)
	}
	if out.TraceID != "t1" || out.Fingerprint != "ip|ua" || len(out.IDs) != 2 || out.IDs[0].Value != "a" {
		t.Errorf("round-trip mismatch: %+v", out)
	}
}

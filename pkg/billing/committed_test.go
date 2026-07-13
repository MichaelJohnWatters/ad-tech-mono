package billing

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

// newCountedEngine builds an engine wired to a shared committed counter,
// simulating one reporting replica in a multi-pod deployment.
func newCountedEngine(counter CommittedCounter, clk clock.Clock) *Engine {
	e := NewEngine(NewMemoryLedger(), NewContractStore(), clk, logger.New("billing-test"))
	e.SetCommittedCounter(counter)
	return e
}

// cpmEvents returns a deterministic set of CPM impression spend events across a
// few campaigns, the exact shape the simulator's load test produces.
func cpmEvents() []SpendEvent {
	prices := []struct {
		campaign string
		price    float64
	}{
		{"camp-a", 0.004}, {"camp-b", 0.006}, {"camp-a", 0.005},
		{"camp-c", 0.010}, {"camp-b", 0.003}, {"camp-a", 0.007},
		{"camp-c", 0.002}, {"camp-b", 0.009},
	}
	out := make([]SpendEvent, len(prices))
	for i, p := range prices {
		out[i] = SpendEvent{
			TraceID: "t-" + p.campaign + "-" + string(rune('0'+i)), CampaignID: p.campaign,
			PublisherID: "pub-1", AdvertiserID: "adv-1", ClearingPrice: p.price,
			Currency: "USD", BidModel: BidCPM, EventType: "impression",
		}
	}
	return out
}

// TestCommittedCounter_MultiReplicaEqualsSingle is the core multi-replica
// correctness property: splitting the event stream across TWO engines that share
// one committed counter (as N reporting pods behind a load-balanced NATS
// consumer would) yields exactly the same committed snapshot as a single engine
// that processed the whole stream. This is what makes reporting horizontally
// scalable without fragmenting pacing.
func TestCommittedCounter_MultiReplicaEqualsSingle(t *testing.T) {
	clk := clock.NewFake(time.Now())
	events := cpmEvents()

	// Baseline: one engine, no shared counter, sees everything (in-memory snapshot).
	single := NewEngine(NewMemoryLedger(), NewContractStore(), clk, logger.New("billing-test"))
	if _, err := single.ProcessBatch(context.Background(), events); err != nil {
		t.Fatalf("single ProcessBatch: %v", err)
	}
	want := single.SnapshotCommitted()

	// Two replicas sharing one counter; the stream is split round-robin between
	// them, so neither sees the whole picture on its own.
	counter := NewMemoryCommittedCounter()
	podA := newCountedEngine(counter, clk)
	podB := newCountedEngine(counter, clk)
	var batchA, batchB []SpendEvent
	for i, ev := range events {
		if i%2 == 0 {
			batchA = append(batchA, ev)
		} else {
			batchB = append(batchB, ev)
		}
	}
	if _, err := podA.ProcessBatch(context.Background(), batchA); err != nil {
		t.Fatalf("podA ProcessBatch: %v", err)
	}
	if _, err := podB.ProcessBatch(context.Background(), batchB); err != nil {
		t.Fatalf("podB ProcessBatch: %v", err)
	}

	// Each pod reads the SAME combined total from the shared counter.
	got := podA.SnapshotCommitted()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("combined committed = %v, want %v (must equal single-engine total)", got, want)
	}
	if gotB := podB.SnapshotCommitted(); !reflect.DeepEqual(gotB, want) {
		t.Fatalf("podB sees %v, want %v (both replicas must see the same combined total)", gotB, want)
	}
}

// TestCommittedCounter_ProcessEventPathAlsoMirrors confirms the per-event
// (non-batch) CPM path also feeds the shared counter, so a mixed deployment
// (batch impression consumer + any per-event caller) still combines correctly.
func TestCommittedCounter_ProcessEventPathAlsoMirrors(t *testing.T) {
	clk := clock.NewFake(time.Now())
	counter := NewMemoryCommittedCounter()
	podA := newCountedEngine(counter, clk)
	podB := newCountedEngine(counter, clk)

	// $0.005 CPM on podA, $0.003 CPM on podB, same campaign.
	if _, err := podA.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "t1", CampaignID: "camp-x", PublisherID: "pub-1", AdvertiserID: "adv-1",
		ClearingPrice: 0.005, Currency: "USD", BidModel: BidCPM, EventType: "impression",
	}); err != nil {
		t.Fatalf("podA: %v", err)
	}
	if _, err := podB.ProcessEvent(context.Background(), SpendEvent{
		TraceID: "t2", CampaignID: "camp-x", PublisherID: "pub-1", AdvertiserID: "adv-1",
		ClearingPrice: 0.003, Currency: "USD", BidModel: BidCPM, EventType: "impression",
	}); err != nil {
		t.Fatalf("podB: %v", err)
	}

	got := podA.SnapshotCommitted()
	want := map[string]int64{"camp-x": toMicros(0.005) + toMicros(0.003)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("combined committed = %v, want %v", got, want)
	}
}

// TestCommittedCounter_ReconcileSweepsDrift proves the self-healing layer: if the
// additive counter drifts (a delta is lost — modelled here by a pod's AddDelta
// never happening), a Reconcile with the authoritative store total resets it to
// truth. Without reconcile the drift would persist (additive counters can't
// self-correct), causing overspend.
func TestCommittedCounter_ReconcileSweepsDrift(t *testing.T) {
	clk := clock.NewFake(time.Now())
	counter := NewMemoryCommittedCounter()
	pod := newCountedEngine(counter, clk)

	// Bill $0.006 — counter reflects it.
	if _, err := pod.ProcessBatch(context.Background(), []SpendEvent{{
		TraceID: "t1", CampaignID: "camp-a", PublisherID: "pub-1", AdvertiserID: "adv-1",
		ClearingPrice: 0.006, Currency: "USD", BidModel: BidCPM, EventType: "impression",
	}}); err != nil {
		t.Fatalf("bill: %v", err)
	}

	// Simulate a LOST delta: another $0.004 truly billed (in the store) but its
	// AddDelta never landed. The counter now under-counts (0.006 vs true 0.010).
	day := dayKey(clk.Now())
	drifted, _ := counter.Snapshot(context.Background(), day)
	if drifted["camp-a"] != toMicros(0.006) {
		t.Fatalf("pre-reconcile = %d, want %d", drifted["camp-a"], toMicros(0.006))
	}

	// The periodic store reconcile recomputes the authoritative total and resets.
	authoritative := map[string]int64{"camp-a": toMicros(0.010)}
	if err := counter.Reconcile(context.Background(), day, authoritative); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := pod.SnapshotCommitted(); got["camp-a"] != toMicros(0.010) {
		t.Fatalf("post-reconcile = %d, want %d (drift not corrected)", got["camp-a"], toMicros(0.010))
	}
}

// TestMemoryCommittedCounter_Additive is a direct unit test of the additive +
// reconcile semantics the interface promises.
func TestMemoryCommittedCounter_Additive(t *testing.T) {
	c := NewMemoryCommittedCounter()
	ctx := context.Background()
	day := "2026-07-13"

	// Two pods add to the same campaign; a third campaign from one pod only.
	if err := c.AddDelta(ctx, day, map[string]int64{"a": 100, "b": 50}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddDelta(ctx, day, map[string]int64{"a": 25}); err != nil {
		t.Fatal(err)
	}
	// Negative delta (a swept reserve) lowers the total.
	if err := c.AddDelta(ctx, day, map[string]int64{"b": -20}); err != nil {
		t.Fatal(err)
	}

	got, _ := c.Snapshot(ctx, day)
	want := map[string]int64{"a": 125, "b": 30}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("additive snapshot = %v, want %v", got, want)
	}

	// Reconcile overwrites only the campaigns present; "a" resets, "b" untouched.
	if err := c.Reconcile(ctx, day, map[string]int64{"a": 200}); err != nil {
		t.Fatal(err)
	}
	got, _ = c.Snapshot(ctx, day)
	want = map[string]int64{"a": 200, "b": 30}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("post-reconcile snapshot = %v, want %v", got, want)
	}

	// Empty-day and zero/empty deltas are no-ops.
	if err := c.AddDelta(ctx, day, map[string]int64{"": 999, "a": 0}); err != nil {
		t.Fatal(err)
	}
	got, _ = c.Snapshot(ctx, day)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("no-op deltas changed snapshot: %v, want %v", got, want)
	}
}

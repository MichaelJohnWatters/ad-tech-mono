package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeBatchInserter records bulk-insert calls and can be told to fail, so a
// test can prove batching (one call, N rows) and the nak-all-on-failure path.
type fakeBatchInserter struct {
	mu    sync.Mutex
	calls int
	rows  int
	fail  bool
}

func (f *fakeBatchInserter) record(n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("clickhouse down")
	}
	f.calls++
	f.rows += n
	return nil
}

func (f *fakeBatchInserter) InsertImpressions(_ context.Context, es []*analytics.ImpressionEvent) error {
	return f.record(len(es))
}
func (f *fakeBatchInserter) InsertClicks(_ context.Context, es []*analytics.ClickEvent) error {
	return f.record(len(es))
}
func (f *fakeBatchInserter) InsertConversions(_ context.Context, es []*analytics.ConversionEvent) error {
	return f.record(len(es))
}
func (f *fakeBatchInserter) InsertViews(_ context.Context, es []*analytics.ViewEvent) error {
	return f.record(len(es))
}
func (f *fakeBatchInserter) InsertAuctions(_ context.Context, es []*analytics.AuctionEvent) error {
	return f.record(len(es))
}
func (f *fakeBatchInserter) InsertAuctionWins(_ context.Context, es []*analytics.AuctionWinEvent) error {
	return f.record(len(es))
}
func (f *fakeBatchInserter) InsertMediaEvents(_ context.Context, es []*analytics.MediaEvent) error {
	return f.record(len(es))
}

// fakeDedup is an in-memory events.DedupStore that records unmarks.
type fakeDedup struct {
	mu       sync.Mutex
	seen     map[string]bool
	unmarks  int
	failMark bool
}

func newFakeDedup() *fakeDedup { return &fakeDedup{seen: map[string]bool{}} }

func (d *fakeDedup) MarkProcessed(_ context.Context, key string, _ time.Duration) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failMark {
		return false, errors.New("redis down")
	}
	if d.seen[key] {
		return false, nil
	}
	d.seen[key] = true
	return true, nil
}

func (d *fakeDedup) UnmarkProcessed(_ context.Context, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.seen, key)
	d.unmarks++
	return nil
}

// ackState aggregates ack/nak counts across a batch of messages.
type ackState struct{ acks, naks int32 }

func impMsg(id, trace string, st *ackState) *events.Message {
	data, _ := json.Marshal(analytics.ImpressionEvent{
		TraceID: trace, CampaignID: "c1", PublisherID: "pub1", AccountID: "acct1",
		ClearingPriceUSD: 1.0, BidModel: "cpm",
	})
	return events.NewMessage(events.SubjectImpression, data, "", id,
		func() error { atomic.AddInt32(&st.acks, 1); return nil },
		func() error { atomic.AddInt32(&st.naks, 1); return nil },
	)
}

func newBatchConsumer(fi analytics.BatchInserter, fd events.DedupStore, eng *billing.Engine) *EventConsumer {
	c := NewEventConsumer(testLog(), analytics.NewMemory(), eng)
	c.EnableBatchConsumer(fi, fd, time.Hour)
	return c
}

// (a) A fetch of N messages becomes ONE InsertImpressions call of N rows, and
// every message is acked exactly once.
func TestBatch_GroupsIntoOneInsert(t *testing.T) {
	fi := &fakeBatchInserter{}
	c := newBatchConsumer(fi, newFakeDedup(), nil)
	st := &ackState{}
	msgs := make([]*events.Message, 10)
	for i := range msgs {
		msgs[i] = impMsg(fmt.Sprintf("seq-%d", i), fmt.Sprintf("t%d", i), st)
	}
	if err := c.handleImpressionBatch(context.Background(), msgs); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if fi.calls != 1 {
		t.Errorf("InsertImpressions calls = %d, want 1 (one batch, not per-row)", fi.calls)
	}
	if fi.rows != 10 {
		t.Errorf("rows inserted = %d, want 10", fi.rows)
	}
	if st.acks != 10 || st.naks != 0 {
		t.Errorf("acks=%d naks=%d, want 10/0", st.acks, st.naks)
	}
}

// (b) Insert failure → nothing acked, every message naked, dedup marks rolled
// back, and NOTHING billed (bill-after-durable).
func TestBatch_InsertFailure_NaksAll_NoBill(t *testing.T) {
	ledger := billing.NewMemoryLedger()
	eng := billing.NewEngine(ledger, billing.NewContractStore(), clock.Real{}, testLog())
	fi := &fakeBatchInserter{fail: true}
	fd := newFakeDedup()
	c := newBatchConsumer(fi, fd, eng)
	st := &ackState{}
	const n = 5
	msgs := make([]*events.Message, n)
	for i := range msgs {
		msgs[i] = impMsg(fmt.Sprintf("seq-%d", i), fmt.Sprintf("t%d", i), st)
	}
	if err := c.handleImpressionBatch(context.Background(), msgs); err == nil {
		t.Fatal("want error on insert failure")
	}
	if st.acks != 0 {
		t.Errorf("acks=%d, want 0 (nothing acked when insert fails)", st.acks)
	}
	if st.naks != n {
		t.Errorf("naks=%d, want %d (whole batch redelivered)", st.naks, n)
	}
	if fd.unmarks != n {
		t.Errorf("dedup unmarks=%d, want %d (marks rolled back for retry)", fd.unmarks, n)
	}
	if got := len(ledger.Entries()); got != 0 {
		t.Errorf("ledger entries=%d, want 0 (must not bill before durable write)", got)
	}
}

// (c) Success → each surviving event billed exactly once, after the durable write.
func TestBatch_Success_BillsEach(t *testing.T) {
	ledger := billing.NewMemoryLedger()
	eng := billing.NewEngine(ledger, billing.NewContractStore(), clock.Real{}, testLog())
	c := newBatchConsumer(&fakeBatchInserter{}, newFakeDedup(), eng)
	st := &ackState{}
	const n = 3
	msgs := make([]*events.Message, n)
	for i := range msgs {
		msgs[i] = impMsg(fmt.Sprintf("seq-%d", i), fmt.Sprintf("t%d", i), st)
	}
	if err := c.handleImpressionBatch(context.Background(), msgs); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if got := len(ledger.Entries()); got != n {
		t.Errorf("ledger entries=%d, want %d (one bill per event)", got, n)
	}
	if st.acks != n {
		t.Errorf("acks=%d, want %d", st.acks, n)
	}
}

// (d) Idempotency: replaying the same message IDs inserts 0 and bills 0 the
// second time; the duplicates are acked.
func TestBatch_Dedup_SecondPassNoInsertNoBill(t *testing.T) {
	ledger := billing.NewMemoryLedger()
	eng := billing.NewEngine(ledger, billing.NewContractStore(), clock.Real{}, testLog())
	fi := &fakeBatchInserter{}
	fd := newFakeDedup()
	c := newBatchConsumer(fi, fd, eng)

	build := func(st *ackState) []*events.Message {
		msgs := make([]*events.Message, 3)
		for i := range msgs {
			msgs[i] = impMsg(fmt.Sprintf("seq-%d", i), fmt.Sprintf("t%d", i), st)
		}
		return msgs
	}

	st1 := &ackState{}
	if err := c.handleImpressionBatch(context.Background(), build(st1)); err != nil {
		t.Fatalf("pass 1 err: %v", err)
	}
	if fi.calls != 1 || fi.rows != 3 || len(ledger.Entries()) != 3 {
		t.Fatalf("pass 1: calls=%d rows=%d entries=%d, want 1/3/3", fi.calls, fi.rows, len(ledger.Entries()))
	}

	// Second pass: SAME message IDs (redelivery).
	st2 := &ackState{}
	if err := c.handleImpressionBatch(context.Background(), build(st2)); err != nil {
		t.Fatalf("pass 2 err: %v", err)
	}
	if fi.calls != 1 {
		t.Errorf("after replay InsertImpressions calls=%d, want still 1 (no re-insert)", fi.calls)
	}
	if got := len(ledger.Entries()); got != 3 {
		t.Errorf("after replay ledger entries=%d, want still 3 (no double-bill)", got)
	}
	if st2.acks != 3 || st2.naks != 0 {
		t.Errorf("replay acks=%d naks=%d, want 3/0 (dupes acked)", st2.acks, st2.naks)
	}
}

// (e) Poison payload is acked and excluded from the insert; good rows still land.
func TestBatch_PoisonExcluded(t *testing.T) {
	fi := &fakeBatchInserter{}
	c := newBatchConsumer(fi, newFakeDedup(), nil)
	st := &ackState{}
	good1 := impMsg("seq-1", "t1", st)
	good2 := impMsg("seq-2", "t2", st)
	bad := events.NewMessage(events.SubjectImpression, []byte("{not json"), "", "seq-3",
		func() error { atomic.AddInt32(&st.acks, 1); return nil },
		func() error { atomic.AddInt32(&st.naks, 1); return nil },
	)
	if err := c.handleImpressionBatch(context.Background(), []*events.Message{good1, bad, good2}); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if fi.calls != 1 || fi.rows != 2 {
		t.Errorf("calls=%d rows=%d, want 1/2 (poison excluded)", fi.calls, fi.rows)
	}
	if st.acks != 3 || st.naks != 0 {
		t.Errorf("acks=%d naks=%d, want 3/0 (poison acked+dropped, goods acked)", st.acks, st.naks)
	}
}

// Dedup-store failure naks the message (fail-closed) rather than risk a
// double-count: no insert, no bill.
func TestBatch_DedupError_Naks(t *testing.T) {
	fi := &fakeBatchInserter{}
	fd := newFakeDedup()
	fd.failMark = true
	c := newBatchConsumer(fi, fd, nil)
	st := &ackState{}
	msgs := []*events.Message{impMsg("seq-1", "t1", st), impMsg("seq-2", "t2", st)}
	if err := c.handleImpressionBatch(context.Background(), msgs); err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if fi.calls != 0 {
		t.Errorf("insert calls=%d, want 0 (dedup unavailable → don't process)", fi.calls)
	}
	if st.naks != 2 || st.acks != 0 {
		t.Errorf("acks=%d naks=%d, want 0/2", st.acks, st.naks)
	}
}

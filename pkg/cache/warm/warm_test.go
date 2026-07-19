package warm

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

type widget struct {
	ID   string
	Name string
}

type fakeLoader struct {
	mu     sync.Mutex
	rows   []widget
	calls  atomic.Int32
	errOn  int32 // call number to return an error on (0 = never)
	errVal error
}

func (f *fakeLoader) set(rows []widget) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = rows
}

func (f *fakeLoader) LoadAll(_ context.Context) ([]widget, error) {
	n := f.calls.Add(1)
	if f.errOn != 0 && n == f.errOn {
		return nil, f.errVal
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]widget, len(f.rows))
	copy(cp, f.rows)
	return cp, nil
}

func (f *fakeLoader) KeyOf(w widget) string { return w.ID }

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(nopWriter{}, nil))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestCache_InitialLoadSync(t *testing.T) {
	loader := &fakeLoader{}
	loader.set([]widget{{ID: "a", Name: "apple"}, {ID: "b", Name: "banana"}})

	c := New(Config[widget]{
		Name:         "widgets",
		Loader:       loader,
		Clock:        clock.Real{},
		PollInterval: time.Hour, // ensure the ticker doesn't fire during the test
		Log:          newTestLogger(),
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer c.Stop()

	if c.Len() != 2 {
		t.Errorf("Len = %d, want 2", c.Len())
	}
	if v, ok := c.ByID("a"); !ok || v.Name != "apple" {
		t.Errorf("ByID(a) = %v, %v", v, ok)
	}
}

func TestCache_StartReturnsLoaderError(t *testing.T) {
	loader := &fakeLoader{errOn: 1, errVal: errors.New("boom")}
	c := New(Config[widget]{
		Name:         "widgets",
		Loader:       loader,
		Clock:        clock.Real{},
		PollInterval: time.Hour,
		Log:          newTestLogger(),
	})
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("Start should have returned the loader error")
	}
}

func TestCache_TriggerForcesRefresh(t *testing.T) {
	loader := &fakeLoader{}
	loader.set([]widget{{ID: "a", Name: "v1"}})

	c := New(Config[widget]{
		Name:         "widgets",
		Loader:       loader,
		Clock:        clock.Real{},
		PollInterval: time.Hour,
		Log:          newTestLogger(),
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	loader.set([]widget{{ID: "a", Name: "v2"}})
	c.Trigger()

	// Trigger is async; wait until the value updates.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if v, _ := c.ByID("a"); v.Name == "v2" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("cache did not refresh after Trigger")
}

func TestCache_NATSInvalidateTriggersRefresh(t *testing.T) {
	loader := &fakeLoader{}
	loader.set([]widget{{ID: "a", Name: "v1"}})
	bus := events.NewMemoryBus()

	c := New(Config[widget]{
		Name:              "widgets",
		Loader:            loader,
		Clock:             clock.Real{},
		Bus:               bus,
		InvalidateSubject: "test.invalidate.widgets",
		PollInterval:      time.Hour,
		Log:               newTestLogger(),
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	loader.set([]widget{{ID: "a", Name: "v2"}})
	if err := bus.Publish(context.Background(), "test.invalidate.widgets", []byte("{}")); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if v, _ := c.ByID("a"); v.Name == "v2" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("cache did not refresh after NATS invalidate")
}

// flakyBus fails the first N Subscribe calls (simulating JetStream briefly
// unavailable at boot), then delegates to the wrapped bus.
type flakyBus struct {
	events.EventBus
	failsLeft atomic.Int32
}

func (b *flakyBus) Subscribe(ctx context.Context, subject, group string, h events.Handler) error {
	if b.failsLeft.Load() > 0 {
		b.failsLeft.Add(-1)
		return errors.New("jetstream temporarily unavailable")
	}
	return b.EventBus.Subscribe(ctx, subject, group, h)
}

// TestCache_ResubscribesAfterInitialFailure: when the boot subscribe fails, the
// cache must retry in the background and, on success, catch up via a refresh —
// rather than staying poll-only until the pod restarts. PollInterval is an hour
// so the ONLY path to the new value is the re-subscribe's catch-up Trigger.
func TestCache_ResubscribesAfterInitialFailure(t *testing.T) {
	loader := &fakeLoader{}
	loader.set([]widget{{ID: "a", Name: "v1"}})
	bus := &flakyBus{EventBus: events.NewMemoryBus()}
	bus.failsLeft.Store(1) // fail the boot subscribe; the retry succeeds

	c := New(Config[widget]{
		Name:                "widgets",
		Loader:              loader,
		Clock:               clock.Real{},
		Bus:                 bus,
		InvalidateSubject:   "test.invalidate.widgets",
		PollInterval:        time.Hour,
		ResubscribeInterval: 10 * time.Millisecond,
		Log:                 newTestLogger(),
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	// New value set after Start; only the re-subscribe catch-up can surface it.
	loader.set([]widget{{ID: "a", Name: "v2"}})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, _ := c.ByID("a"); v.Name == "v2" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cache did not re-subscribe + refresh after the initial subscribe failure")
}

func TestCache_AllReturnsSnapshot(t *testing.T) {
	loader := &fakeLoader{}
	loader.set([]widget{{ID: "a"}, {ID: "b"}, {ID: "c"}})

	c := New(Config[widget]{
		Name: "w", Loader: loader, Clock: clock.Real{},
		PollInterval: time.Hour, Log: newTestLogger(),
	})
	_ = c.Start(context.Background())
	defer c.Stop()

	got := c.All()
	if len(got) != 3 {
		t.Errorf("All() len = %d, want 3", len(got))
	}
}

func TestCache_RefreshSynchronouslyReloads(t *testing.T) {
	loader := &fakeLoader{}
	loader.set([]widget{{ID: "a", Name: "v1"}})

	c := New(Config[widget]{
		Name: "w", Loader: loader, Clock: clock.Real{},
		PollInterval: time.Hour, Log: newTestLogger(),
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	loader.set([]widget{{ID: "a", Name: "v2"}, {ID: "b", Name: "v3"}})
	n, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if n != 2 {
		t.Errorf("Refresh returned count %d, want 2", n)
	}
	if v, _ := c.ByID("a"); v.Name != "v2" {
		t.Errorf("after Refresh ByID(a) name = %q, want v2 (sync reload should have completed)", v.Name)
	}
}

func TestRefreshHandler_RefreshesAndReturnsCount(t *testing.T) {
	loader := &fakeLoader{}
	loader.set([]widget{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	c := New(Config[widget]{
		Name: "widgets", Loader: loader, Clock: clock.Real{},
		PollInterval: time.Hour, Log: newTestLogger(),
	})
	_ = c.Start(context.Background())
	defer c.Stop()

	srv := httptest.NewServer(RefreshHandler(c))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Refreshed []struct {
			Cache string `json:"cache"`
			Count int    `json:"count"`
		} `json:"refreshed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Refreshed) != 1 || got.Refreshed[0].Cache != "widgets" || got.Refreshed[0].Count != 3 {
		t.Errorf("response = %+v, want one widgets cache with count 3", got)
	}
}

func TestRefreshHandler_UnknownNameIs404(t *testing.T) {
	loader := &fakeLoader{}
	loader.set([]widget{{ID: "a"}})
	c := New(Config[widget]{
		Name: "widgets", Loader: loader, Clock: clock.Real{},
		PollInterval: time.Hour, Log: newTestLogger(),
	})
	_ = c.Start(context.Background())
	defer c.Stop()

	srv := httptest.NewServer(RefreshHandler(c))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"?name=does-not-exist", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestCache_LastLoadedReportsState(t *testing.T) {
	loader := &fakeLoader{}
	loader.set([]widget{{ID: "a"}})
	c := New(Config[widget]{
		Name: "w", Loader: loader, Clock: clock.Real{},
		PollInterval: time.Hour, Log: newTestLogger(),
	})
	_ = c.Start(context.Background())
	defer c.Stop()

	ts, err := c.LastLoaded()
	if err != nil {
		t.Errorf("LastLoaded err = %v", err)
	}
	if ts.IsZero() {
		t.Error("LastLoaded ts is zero")
	}
}

// singleFakeLoader adds LoadOne so it satisfies SingleLoader — lets us drive
// applyOne directly and assert the targeted path does NOT call LoadAll.
type singleFakeLoader struct {
	fakeLoader
	loadOneCalls atomic.Int32
	one          map[string]widget // key -> value; absent => found=false (evict)
}

func (f *singleFakeLoader) LoadOne(_ context.Context, key string) (widget, bool, error) {
	f.loadOneCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.one[key]
	return w, ok, nil
}

func TestCache_TargetedInvalidate_UpsertsAndEvicts_NoFullReload(t *testing.T) {
	loader := &singleFakeLoader{one: map[string]widget{}}
	loader.set([]widget{{ID: "a", Name: "a1"}, {ID: "b", Name: "b2"}})
	c := New(Config[widget]{Name: "w", Loader: loader, Clock: clock.Real{}, PollInterval: time.Hour, Log: newTestLogger()})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	loadsAfterStart := loader.calls.Load()

	// Upsert "a" via a targeted invalidate (id in payload) — must use LoadOne,
	// not a full LoadAll.
	loader.one["a"] = widget{ID: "a", Name: "a99"}
	_ = c.onInvalidate(context.Background(), events.NewMessage("s", []byte(`{"id":"a"}`), "", "m1", func() error { return nil }, func() error { return nil }))
	if got, _ := c.ByID("a"); got.Name != "a99" {
		t.Errorf("targeted upsert: a.Name=%q, want a99", got.Name)
	}
	if b, ok := c.ByID("b"); !ok || b.Name != "b2" {
		t.Errorf("untouched entry b changed: %+v", b)
	}
	if loader.calls.Load() != loadsAfterStart {
		t.Errorf("targeted update triggered a full LoadAll (calls %d -> %d)", loadsAfterStart, loader.calls.Load())
	}
	if loader.loadOneCalls.Load() != 1 {
		t.Errorf("LoadOne calls=%d, want 1", loader.loadOneCalls.Load())
	}

	// Evict: "a" no longer present (e.g. left 'live') → found=false.
	delete(loader.one, "a")
	_ = c.onInvalidate(context.Background(), events.NewMessage("s", []byte(`{"id":"a"}`), "", "m2", func() error { return nil }, func() error { return nil }))
	if _, ok := c.ByID("a"); ok {
		t.Error("targeted evict: a should be gone")
	}

	// No id in payload → full reload (Trigger path).
	_ = c.onInvalidate(context.Background(), events.NewMessage("s", []byte(`{"op":"refresh"}`), "", "m3", func() error { return nil }, func() error { return nil }))
	// Trigger is async; give the poll goroutine a moment.
	time.Sleep(50 * time.Millisecond)
	if loader.calls.Load() == loadsAfterStart {
		t.Error("id-less invalidate should have triggered a full LoadAll")
	}
}

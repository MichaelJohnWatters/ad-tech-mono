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

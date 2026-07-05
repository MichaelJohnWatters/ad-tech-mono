package webhooks

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeStore returns a fixed subscription set and captures delivery records.
type fakeStore struct {
	mu      sync.Mutex
	subs    map[string][]Subscription // keyed by eventType
	records []Delivery
	lookErr error
}

func (f *fakeStore) ActiveForEvent(_ context.Context, _, eventType string) ([]Subscription, error) {
	if f.lookErr != nil {
		return nil, f.lookErr
	}
	return f.subs[eventType], nil
}

func (f *fakeStore) RecordDelivery(_ context.Context, d Delivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, d)
	return nil
}

func (f *fakeStore) recorded() []Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Delivery, len(f.records))
	copy(out, f.records)
	return out
}

func newTestDispatcher(store Store) *Dispatcher {
	return &Dispatcher{
		Store:       store,
		HTTP:        &http.Client{Timeout: 2 * time.Second},
		MaxAttempts: 3,
		Backoff:     func(int) time.Duration { return 0 },
		Now:         func() time.Time { return time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC) },
		Log:         quietLog(),
		sleep:       func(context.Context, time.Duration) {},
	}
}

func TestDispatch_DeliversSignedEnvelope(t *testing.T) {
	type received struct {
		body []byte
		sig  string
		ev   string
	}
	got := make(chan received, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- received{body: b, sig: r.Header.Get(HeaderSignature), ev: r.Header.Get(HeaderEvent)}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := &fakeStore{subs: map[string][]Subscription{
		"budget.depleted": {{ID: "wh-1", URL: srv.URL, Secret: "shhh"}},
	}}
	d := newTestDispatcher(store)

	err := d.Dispatch(context.Background(), Event{
		Type:      "budget.depleted",
		AccountID: "acct-1",
		Data:      json.RawMessage(`{"campaign_id":"c1"}`),
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	select {
	case r := <-got:
		// Envelope shape.
		var env envelope
		if err := json.Unmarshal(r.body, &env); err != nil {
			t.Fatalf("envelope not JSON: %v", err)
		}
		if env.Event != "budget.depleted" || env.AccountID != "acct-1" {
			t.Errorf("bad envelope: %+v", env)
		}
		if string(env.Data) != `{"campaign_id":"c1"}` {
			t.Errorf("data not passed through: %s", env.Data)
		}
		// Signature verifies against the raw body.
		if want := Sign("shhh", r.body); r.sig != want {
			t.Errorf("signature = %q, want %q", r.sig, want)
		}
		if r.ev != "budget.depleted" {
			t.Errorf("event header = %q", r.ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery received")
	}

	recs := store.recorded()
	if len(recs) != 1 || !recs[0].Success || recs[0].Attempt != 1 {
		t.Errorf("expected 1 successful attempt, got %+v", recs)
	}
}

func TestDispatch_RetriesThenSucceeds(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := &fakeStore{subs: map[string][]Subscription{
		"campaign.state_changed": {{ID: "wh-1", URL: srv.URL, Secret: "s"}},
	}}
	d := newTestDispatcher(store)

	if err := d.Dispatch(context.Background(), Event{Type: "campaign.state_changed", AccountID: "a", Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	recs := store.recorded()
	if len(recs) != 3 {
		t.Fatalf("expected 3 recorded attempts, got %d: %+v", len(recs), recs)
	}
	if recs[0].Success || recs[1].Success || !recs[2].Success {
		t.Errorf("expected fail, fail, success: %+v", recs)
	}
}

func TestDispatch_GivesUpAfterMaxAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	store := &fakeStore{subs: map[string][]Subscription{
		"balance.depleted": {{ID: "wh-1", URL: srv.URL, Secret: "s"}},
	}}
	d := newTestDispatcher(store)
	_ = d.Dispatch(context.Background(), Event{Type: "balance.depleted", AccountID: "a", Data: json.RawMessage(`{}`)})

	recs := store.recorded()
	if len(recs) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(recs))
	}
	for i, r := range recs {
		if r.Success {
			t.Errorf("attempt %d unexpectedly succeeded", i+1)
		}
		if r.ResponseStatus != http.StatusBadGateway {
			t.Errorf("attempt %d status = %d", i+1, r.ResponseStatus)
		}
	}
}

func TestDispatch_NoSubscriptionsIsNoop(t *testing.T) {
	store := &fakeStore{subs: map[string][]Subscription{}}
	d := newTestDispatcher(store)
	if err := d.Dispatch(context.Background(), Event{Type: "budget.depleted", AccountID: "a", Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(store.recorded()) != 0 {
		t.Errorf("expected no deliveries")
	}
}

func TestDispatch_FanOutToMultiple(t *testing.T) {
	var hitA, hitB bool
	var mu sync.Mutex
	mk := func(flag *bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			*flag = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
	}
	a, b := mk(&hitA), mk(&hitB)
	defer a.Close()
	defer b.Close()

	store := &fakeStore{subs: map[string][]Subscription{
		"budget.depleted": {{ID: "wh-a", URL: a.URL, Secret: "s"}, {ID: "wh-b", URL: b.URL, Secret: "s"}},
	}}
	d := newTestDispatcher(store)
	_ = d.Dispatch(context.Background(), Event{Type: "budget.depleted", AccountID: "a", Data: json.RawMessage(`{}`)})

	mu.Lock()
	defer mu.Unlock()
	if !hitA || !hitB {
		t.Errorf("expected both endpoints hit: a=%v b=%v", hitA, hitB)
	}
}

func TestDispatch_ValidatesEvent(t *testing.T) {
	d := newTestDispatcher(&fakeStore{})
	if err := d.Dispatch(context.Background(), Event{Type: "", AccountID: "a"}); err == nil {
		t.Error("expected error for missing type")
	}
	if err := d.Dispatch(context.Background(), Event{Type: "x", AccountID: ""}); err == nil {
		t.Error("expected error for missing account")
	}
}

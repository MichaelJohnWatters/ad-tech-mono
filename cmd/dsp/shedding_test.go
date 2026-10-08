package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func shedCount(t *testing.T, c prometheus.Counter) int {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return int(m.GetCounter().GetValue())
}

// TestShedBids: requests over the in-flight cap get an instant 204 and are
// counted; requests under it serve normally; cap 0 disables entirely.
func TestShedBids(t *testing.T) {
	const cap = 2
	shed := newShedCounter(prometheus.NewRegistry())
	release := make(chan struct{})
	inHandler := make(chan struct{}, cap)
	blocking := shedBids(func(w http.ResponseWriter, r *http.Request) {
		inHandler <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}, func() int { return cap }, shed)

	// Fill the cap with two in-flight requests.
	var wg sync.WaitGroup
	codes := make([]int, cap)
	for i := 0; i < cap; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			blocking(rec, httptest.NewRequest(http.MethodPost, "/bid", nil))
			codes[i] = rec.Code
		}()
	}
	for i := 0; i < cap; i++ {
		<-inHandler // both are genuinely inside the handler
	}

	// The cap+1'th request must shed instantly — no queueing.
	rec := httptest.NewRecorder()
	blocking(rec, httptest.NewRequest(http.MethodPost, "/bid", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("over-cap request: want 204, got %d", rec.Code)
	}
	if got := shedCount(t, shed); got != 1 {
		t.Fatalf("shed counter: want 1, got %d", got)
	}

	close(release)
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("under-cap request %d: want 200, got %d", i, c)
		}
	}

	// Capacity released: the next request serves.
	rec = httptest.NewRecorder()
	blocking(rec, httptest.NewRequest(http.MethodPost, "/bid", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("post-release request: want 200, got %d", rec.Code)
	}

	// Cap 0 = disabled: never sheds.
	open := shedBids(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, func() int { return 0 }, shed)
	rec = httptest.NewRecorder()
	open(rec, httptest.NewRequest(http.MethodPost, "/bid", nil))
	if rec.Code != http.StatusOK || shedCount(t, shed) != 1 {
		t.Fatalf("cap 0 must disable shedding (code %d, sheds %d)", rec.Code, shedCount(t, shed))
	}
}

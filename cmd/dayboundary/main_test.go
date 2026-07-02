package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

func dbTestLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeFlightStore struct {
	activate, end       []postgres.FlightTransition
	activateErr, endErr error
}

func (f *fakeFlightStore) ActivateFlights(_ context.Context, _ time.Time) ([]postgres.FlightTransition, error) {
	return f.activate, f.activateErr
}
func (f *fakeFlightStore) EndFlights(_ context.Context, _ time.Time) ([]postgres.FlightTransition, error) {
	return f.end, f.endErr
}

// runDayBoundary counts transitions and publishes one CampaignStateEvent per
// cascaded line item plus a single campaign cache-invalidate.
func TestRunDayBoundary_PublishesTransitions(t *testing.T) {
	store := &fakeFlightStore{
		activate: []postgres.FlightTransition{{LineItemID: "li1", AccountID: "a1", OldStatus: "approved", NewStatus: "live"}},
		end: []postgres.FlightTransition{
			{LineItemID: "li2", AccountID: "a2", OldStatus: "live", NewStatus: "ended"},
			{LineItemID: "li3", AccountID: "a2", OldStatus: "paused", NewStatus: "ended"},
		},
	}
	bus := events.NewMemoryBus()
	var states, invalidates int32
	_ = bus.Subscribe(context.Background(), events.SubjectCampaignStateChanged, "test",
		func(context.Context, *events.Message) error { atomic.AddInt32(&states, 1); return nil })
	_ = bus.Subscribe(context.Background(), events.SubjectCacheInvalidateCampaigns, "test",
		func(context.Context, *events.Message) error { atomic.AddInt32(&invalidates, 1); return nil })

	res := runDayBoundary(context.Background(), store, bus, dbTestLog(), time.Now())

	if res.CampaignsStarted != 1 || res.CampaignsEnded != 2 {
		t.Fatalf("counts = started %d ended %d, want 1/2", res.CampaignsStarted, res.CampaignsEnded)
	}
	if states != 3 {
		t.Errorf("state events = %d, want 3 (one per cascaded line item)", states)
	}
	if invalidates != 1 {
		t.Errorf("cache invalidates = %d, want 1", invalidates)
	}
}

// A store error is logged and skipped — no events, no crash, other steps run.
func TestRunDayBoundary_StoreErrorIsNonFatal(t *testing.T) {
	store := &fakeFlightStore{
		activateErr: errors.New("db down"),
		end:         []postgres.FlightTransition{{LineItemID: "li2", AccountID: "a2", OldStatus: "live", NewStatus: "ended"}},
	}
	bus := events.NewMemoryBus()
	var states int32
	_ = bus.Subscribe(context.Background(), events.SubjectCampaignStateChanged, "test",
		func(context.Context, *events.Message) error { atomic.AddInt32(&states, 1); return nil })

	res := runDayBoundary(context.Background(), store, bus, dbTestLog(), time.Now())
	if res.CampaignsStarted != 0 {
		t.Errorf("started = %d, want 0 (activate failed)", res.CampaignsStarted)
	}
	if res.CampaignsEnded != 1 {
		t.Errorf("ended = %d, want 1 (end still ran)", res.CampaignsEnded)
	}
	if states != 1 {
		t.Errorf("state events = %d, want 1 (only the successful end)", states)
	}
}

// nil bus (no NATS) must not panic — transitions still counted.
func TestRunDayBoundary_NilBus(t *testing.T) {
	store := &fakeFlightStore{activate: []postgres.FlightTransition{{LineItemID: "li1", AccountID: "a1", OldStatus: "approved", NewStatus: "live"}}}
	res := runDayBoundary(context.Background(), store, nil, dbTestLog(), time.Now())
	if res.CampaignsStarted != 1 {
		t.Errorf("started = %d, want 1", res.CampaignsStarted)
	}
}

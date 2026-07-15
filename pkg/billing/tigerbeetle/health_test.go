package tigerbeetle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
)

func TestHealthHealthyClient(t *testing.T) {
	l := New(newFakeClient(), silentLogger())
	if err := l.Health(context.Background()); err != nil {
		t.Fatalf("healthy client: Health() = %v, want nil", err)
	}
}

func TestHealthTransportErrorStreakTripsWedge(t *testing.T) {
	fc := newFakeClient()
	fc.createTransfersErr = errors.New("client wedged")
	l := New(fc, silentLogger())

	entry := billing.LedgerEntry{
		Type:          billing.EntrySpend,
		DebitAccount:  "advertiser:" + advUUID("health"),
		CreditAccount: "publisher:" + pubUUID("health"),
		TraceID:       "trace-health-streak",
	}
	for i := 0; i < wedgedStreakThreshold; i++ {
		l.Record(entry)
	}

	err := l.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "consecutive transport errors") {
		t.Fatalf("after %d transport errors: Health() = %v, want wedged", wedgedStreakThreshold, err)
	}
}

func TestHealthStreakResetsOnSuccess(t *testing.T) {
	fc := newFakeClient()
	fc.createTransfersErr = errors.New("client wedged")
	l := New(fc, silentLogger())

	entry := billing.LedgerEntry{
		Type:          billing.EntrySpend,
		DebitAccount:  "advertiser:" + advUUID("health"),
		CreditAccount: "publisher:" + pubUUID("health"),
		TraceID:       "trace-health-reset",
	}
	for i := 0; i < wedgedStreakThreshold-1; i++ {
		l.Record(entry)
	}
	fc.createTransfersErr = nil // one success breaks the streak
	l.Record(entry)

	if err := l.Health(context.Background()); err != nil {
		t.Fatalf("streak broken by success: Health() = %v, want nil", err)
	}
}

func TestHealthActiveProbeError(t *testing.T) {
	fc := newFakeClient()
	fc.lookupAccountsErr = errors.New("no response")
	l := New(fc, silentLogger())

	err := l.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "health probe") {
		t.Fatalf("probe error: Health() = %v, want probe failure", err)
	}
}

func TestHealthHungProbeReportsWedged(t *testing.T) {
	old := probeTimeout
	probeTimeout = 30 * time.Millisecond
	defer func() { probeTimeout = old }()

	fc := newFakeClient()
	fc.lookupAccountsBlock = make(chan struct{})
	defer close(fc.lookupAccountsBlock) // let the goroutine finish at test end
	l := New(fc, silentLogger())

	err := l.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("hung probe: Health() = %v, want timeout", err)
	}

	// While the first probe is still stuck in flight, subsequent checks must
	// report unhealthy without stacking another probe goroutine.
	err = l.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "hung") {
		t.Fatalf("stuck in-flight probe: Health() = %v, want hung", err)
	}
}

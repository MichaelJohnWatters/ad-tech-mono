package tigerbeetle

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	tbtypes "github.com/tigerbeetle/tigerbeetle-go/pkg/types"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tb"
)

// The TB client has a known wedge mode (observed after VM clock regressions:
// the client-side thread panics or stalls) where every CreateTransfers either
// errors or hangs indefinitely — while the pod stays Ready and billing deltas
// silently read zero. Health() exists so /readyz can surface that state as a
// 503 instead of silent money loss.
//
// Two detectors, either one trips:
//
//   - Passive: a streak of consecutive transport-level client errors
//     (err != nil from CreateTransfers/CreateAccounts — NOT per-transfer
//     result codes, which are domain outcomes). Real traffic feeds this.
//   - Active: a LookupAccounts round-trip, time-bounded. This catches the
//     hang variant with zero traffic. The SDK call cannot be cancelled, so
//     a wedged probe goroutine is left behind and a single-flight latch
//     stops the next probe from stacking another one — while the probe is
//     stuck in flight past its deadline, Health() keeps reporting unhealthy.

// wedgedStreakThreshold is how many consecutive transport errors mark the
// client wedged. Transient single-request hiccups (a TB pod restart) recover
// within one or two requests; the wedge produces an unbroken storm.
const wedgedStreakThreshold = 5

// probeTimeout bounds the active LookupAccounts probe. A healthy in-cluster
// round-trip is sub-millisecond; a wedged client never answers. Var (not
// const) only so tests can shrink it.
var probeTimeout = 2 * time.Second

type healthState struct {
	transportErrStreak atomic.Int64
	probeInFlight      atomic.Bool
	// probeStartedNanos is the wall-clock start of the in-flight probe,
	// 0 when none is running. Read together with probeInFlight to decide
	// whether an in-flight probe has blown its deadline (= hung client).
	probeStartedNanos atomic.Int64
}

// noteTransport records the outcome of one client round-trip for the passive
// detector. Called from every path that talks to the TB client.
func (l *Ledger) noteTransport(err error) {
	if err != nil {
		l.health.transportErrStreak.Add(1)
		return
	}
	l.health.transportErrStreak.Store(0)
}

// Health implements a readiness check for the TB client. It returns nil when
// the client demonstrably works, an error when it is wedged (error storm or
// hung probe). Cheap to call from a k8s probe interval: the active probe is
// single-flight and only launched when no verdict is available without it.
func (l *Ledger) Health(ctx context.Context) error {
	if streak := l.health.transportErrStreak.Load(); streak >= wedgedStreakThreshold {
		return fmt.Errorf("tigerbeetle client wedged: %d consecutive transport errors", streak)
	}

	// A previous probe still in flight past its deadline means the client is
	// hanging — report unhealthy without stacking another goroutine.
	if l.health.probeInFlight.Load() {
		started := l.health.probeStartedNanos.Load()
		if started != 0 && time.Since(time.Unix(0, started)) > probeTimeout {
			return fmt.Errorf("tigerbeetle client wedged: health probe hung >%s", probeTimeout)
		}
		// A concurrent probe is still within its budget — treat as healthy
		// rather than blocking this readiness request behind it.
		return nil
	}

	if !l.health.probeInFlight.CompareAndSwap(false, true) {
		return nil // lost the race to another prober; it will report
	}
	l.health.probeStartedNanos.Store(time.Now().UnixNano())

	done := make(chan error, 1)
	go func() {
		// LookupAccounts of the house account: the cheapest call that
		// exercises the full client request path. "Not found" is fine —
		// transport success is the signal, not the account's existence.
		_, err := l.client.LookupAccounts([]tbtypes.Uint128{tb.HouseAccountID})
		done <- err
		l.health.probeStartedNanos.Store(0)
		l.health.probeInFlight.Store(false)
	}()

	timer := time.NewTimer(probeTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		l.noteTransport(err)
		if err != nil {
			return fmt.Errorf("tigerbeetle health probe: %w", err)
		}
		return nil
	case <-timer.C:
		// The goroutine is left to finish (or hang) on its own; the
		// in-flight latch above keeps subsequent probes from stacking.
		return fmt.Errorf("tigerbeetle client wedged: health probe exceeded %s", probeTimeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

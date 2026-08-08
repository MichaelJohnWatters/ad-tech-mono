package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identityobserve"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// capBus captures published messages for assertions. Implements events.EventBus.
type capBus struct {
	mu   sync.Mutex
	msgs map[string][][]byte
}

func newCapBus() *capBus { return &capBus{msgs: map[string][][]byte{}} }

func (b *capBus) Publish(_ context.Context, subject string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs[subject] = append(b.msgs[subject], data)
	return nil
}
func (b *capBus) Subscribe(context.Context, string, string, events.Handler) error { return nil }
func (b *capBus) Close() error                                                    { return nil }

func (b *capBus) published(subject string) [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.msgs[subject]
}

func TestGatherSignals(t *testing.T) {
	r := httptest.NewRequest("GET", "/serve?hashed_email=E&ifa=D", nil)
	ids := gatherSignals(r, "USER", "U", "")
	want := []string{"USER", "U", "E", "D"} // fixed order
	if len(ids) != len(want) {
		t.Fatalf("got %d ids, want %d: %+v", len(ids), len(want), ids)
	}
	for i, w := range want {
		if ids[i].Value != w {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i].Value, w)
		}
	}
	// dedupe + skip empty
	if got := gatherSignals(httptest.NewRequest("GET", "/serve?hashed_email=SAME", nil), "SAME", "", ""); len(got) != 1 {
		t.Errorf("deduped: got %d, want 1", len(got))
	}
}

func TestRequestFingerprint(t *testing.T) {
	// resolved IP + explicit ?ua
	if fp := requestFingerprint(httptest.NewRequest("GET", "/s?ua=Moz", nil), "1.2.3.4"); fp != "1.2.3.4|Moz" {
		t.Errorf("got %q, want 1.2.3.4|Moz", fp)
	}
	// UA header fallback
	r := httptest.NewRequest("GET", "/s", nil)
	r.Header.Set("User-Agent", "UA")
	if fp := requestFingerprint(r, "9.9.9.9"); fp != "9.9.9.9|UA" {
		t.Errorf("got %q, want 9.9.9.9|UA", fp)
	}
	// missing half → empty (never fingerprint on IP alone, or UA alone)
	if fp := requestFingerprint(httptest.NewRequest("GET", "/s", nil), "1.2.3.4"); fp != "" {
		// no UA header on httptest requests → empty
		t.Errorf("got %q, want empty when UA missing", fp)
	}
	if fp := requestFingerprint(httptest.NewRequest("GET", "/s?ua=Moz", nil), ""); fp != "" {
		t.Errorf("got %q, want empty when IP missing", fp)
	}
}

// TestEndUserIPFn pins the override gate: ?ip= is honoured only for
// allowlisted (private-range, by default) callers; a public caller gets the
// trusted-proxy resolved address no matter what ?ip= claims.
func TestEndUserIPFn(t *testing.T) {
	fn := newEndUserIPFn(config.Load())

	// Private caller (the simulator/harness/SSAI shape) → ?ip= honoured.
	r := httptest.NewRequest("GET", "/s?ip=203.0.113.50", nil)
	r.RemoteAddr = "10.42.0.7:41000"
	if got := fn(r); got != "203.0.113.50" {
		t.Errorf("private caller: got %q, want the ?ip= override", got)
	}
	// Public caller → override IGNORED (a browser must not rotate households).
	r = httptest.NewRequest("GET", "/s?ip=203.0.113.50", nil)
	r.RemoteAddr = "198.51.100.9:41000"
	if got := fn(r); got != "198.51.100.9" {
		t.Errorf("public caller: got %q, want the connection IP", got)
	}
	// Public caller, forged XFF prepend → rightmost (trusted) entry wins.
	r = httptest.NewRequest("GET", "/s", nil)
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.9")
	if got := fn(r); got != "198.51.100.9" {
		t.Errorf("forged XFF: got %q, want the rightmost entry", got)
	}
}

// waitPublished polls for want messages — Observe publishes on a goroutine
// (the serve path must not block on the JetStream ack), so assertions wait.
func waitPublished(t *testing.T, bus *capBus, subject string, want int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		msgs := bus.published(subject)
		if len(msgs) >= want {
			return msgs
		}
		if time.Now().After(deadline) {
			t.Fatalf("published %d messages, want %d", len(msgs), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestIdentityPublisher(t *testing.T) {
	bus := newCapBus()
	p := newIdentityPublisher(bus, quietLog())

	t.Run("publishes an event with ids + fingerprint", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/serve?hashed_email=E&ip=1.2.3.4&ua=Moz", nil)
		p.Observe(r, "USER", "U", "", "1.2.3.4")
		msgs := waitPublished(t, bus, events.SubjectIdentityObserved, 1)
		ev, err := identityobserve.Unmarshal(msgs[0])
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(ev.IDs) != 3 || ev.Fingerprint != "1.2.3.4|Moz" {
			t.Errorf("event = %+v, want 3 ids + fingerprint", ev)
		}
	})

	t.Run("nothing to link → no publish", func(t *testing.T) {
		bus := newCapBus()
		p := newIdentityPublisher(bus, quietLog())
		p.Observe(httptest.NewRequest("GET", "/serve", nil), "solo", "", "", "") // one id, no fp
		time.Sleep(50 * time.Millisecond)                                        // grace for the async goroutine to (not) publish
		if n := len(bus.published(events.SubjectIdentityObserved)); n != 0 {
			t.Errorf("published %d, want 0", n)
		}
	})

	t.Run("single id + fingerprint still publishes (for probabilistic)", func(t *testing.T) {
		bus := newCapBus()
		p := newIdentityPublisher(bus, quietLog())
		p.Observe(httptest.NewRequest("GET", "/serve?ua=Moz", nil), "solo", "", "", "1.2.3.4")
		waitPublished(t, bus, events.SubjectIdentityObserved, 1)
	})

	t.Run("nil publisher is a no-op", func(t *testing.T) {
		var np *identityPublisher
		np.Observe(httptest.NewRequest("GET", "/serve?hashed_email=E&ua=x", nil), "USER", "U", "", "1")
	})
}

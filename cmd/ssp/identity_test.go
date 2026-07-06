package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"

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
	ids := gatherSignals(r, "USER", "U")
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
	if got := gatherSignals(httptest.NewRequest("GET", "/serve?hashed_email=SAME", nil), "SAME", ""); len(got) != 1 {
		t.Errorf("deduped: got %d, want 1", len(got))
	}
}

func TestRequestFingerprint(t *testing.T) {
	// explicit ?ip & ?ua
	if fp := requestFingerprint(httptest.NewRequest("GET", "/s?ip=1.2.3.4&ua=Moz", nil)); fp != "1.2.3.4|Moz" {
		t.Errorf("got %q, want 1.2.3.4|Moz", fp)
	}
	// header fallback
	r := httptest.NewRequest("GET", "/s", nil)
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 1.1.1.1")
	r.Header.Set("User-Agent", "UA")
	if fp := requestFingerprint(r); fp != "9.9.9.9|UA" {
		t.Errorf("got %q, want 9.9.9.9|UA (first XFF hop)", fp)
	}
	// missing half → empty
	if fp := requestFingerprint(httptest.NewRequest("GET", "/s?ip=1.2.3.4", nil)); fp != "" {
		// no UA header on httptest requests → empty
		t.Errorf("got %q, want empty when UA missing", fp)
	}
}

func TestIdentityPublisher(t *testing.T) {
	bus := newCapBus()
	p := newIdentityPublisher(bus, quietLog())

	t.Run("publishes an event with ids + fingerprint", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/serve?hashed_email=E&ip=1.2.3.4&ua=Moz", nil)
		p.Observe(r, "USER", "U")
		msgs := bus.published(events.SubjectIdentityObserved)
		if len(msgs) != 1 {
			t.Fatalf("published %d messages, want 1", len(msgs))
		}
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
		p.Observe(httptest.NewRequest("GET", "/serve", nil), "solo", "") // one id, no fp
		if n := len(bus.published(events.SubjectIdentityObserved)); n != 0 {
			t.Errorf("published %d, want 0", n)
		}
	})

	t.Run("single id + fingerprint still publishes (for probabilistic)", func(t *testing.T) {
		bus := newCapBus()
		p := newIdentityPublisher(bus, quietLog())
		p.Observe(httptest.NewRequest("GET", "/serve?ip=1.2.3.4&ua=Moz", nil), "solo", "")
		if n := len(bus.published(events.SubjectIdentityObserved)); n != 1 {
			t.Errorf("published %d, want 1", n)
		}
	})

	t.Run("nil publisher is a no-op", func(t *testing.T) {
		var np *identityPublisher
		np.Observe(httptest.NewRequest("GET", "/serve?hashed_email=E&ip=1&ua=x", nil), "USER", "U")
	})
}

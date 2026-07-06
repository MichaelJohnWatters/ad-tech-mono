package identityobserve

import (
	"context"
	"sync"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

type capBus struct {
	mu   sync.Mutex
	msgs [][]byte
}

func (b *capBus) Publish(_ context.Context, subject string, data []byte) error {
	if subject == events.SubjectIdentityObserved {
		b.mu.Lock()
		b.msgs = append(b.msgs, data)
		b.mu.Unlock()
	}
	return nil
}
func (b *capBus) Subscribe(context.Context, string, string, events.Handler) error { return nil }
func (b *capBus) Close() error                                                    { return nil }

func TestSignalsFromRequest(t *testing.T) {
	req := &openrtb.BidRequest{
		User: &openrtb.User{
			ID:   "pubuser-1",
			EIDs: []openrtb.EID{openrtb.UID2EID("uid2-tok")},
			Ext:  &openrtb.UserExt{HashedEmail: "sha-abc", PublisherUserID: "pubuser-1"}, // dup value
		},
		Device: &openrtb.Device{IFA: "ifa-1", IP: "1.2.3.4", UA: "Moz"},
	}
	ids, fp := SignalsFromRequest(req)
	// pubuser-1, uid2-tok, sha-abc, ifa-1 — the duplicate publisher_user_id drops.
	wantVals := []string{"pubuser-1", "uid2-tok", "sha-abc", "ifa-1"}
	if len(ids) != len(wantVals) {
		t.Fatalf("got %d ids, want %d: %+v", len(ids), len(wantVals), ids)
	}
	for i, w := range wantVals {
		if ids[i].Value != w {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i].Value, w)
		}
	}
	if fp != "1.2.3.4|Moz" {
		t.Errorf("fingerprint = %q, want 1.2.3.4|Moz", fp)
	}

	t.Run("nil / empty request", func(t *testing.T) {
		if ids, fp := SignalsFromRequest(nil); ids != nil || fp != "" {
			t.Errorf("nil req: got %v, %q", ids, fp)
		}
		if ids, fp := SignalsFromRequest(&openrtb.BidRequest{}); ids != nil || fp != "" {
			t.Errorf("empty req: got %v, %q", ids, fp)
		}
	})

	t.Run("IP without UA yields no fingerprint", func(t *testing.T) {
		_, fp := SignalsFromRequest(&openrtb.BidRequest{Device: &openrtb.Device{IP: "1.2.3.4"}})
		if fp != "" {
			t.Errorf("fingerprint = %q, want empty (no UA)", fp)
		}
	})
}

func TestPublisher(t *testing.T) {
	t.Run("publishes when there's something to link", func(t *testing.T) {
		bus := &capBus{}
		p := NewPublisher(bus, quietLog())
		p.Publish("trace-1", []Signal{sig("a", "uid2"), sig("b", "hashed_email")}, "ip|ua")
		if len(bus.msgs) != 1 {
			t.Fatalf("published %d, want 1", len(bus.msgs))
		}
		ev, _ := Unmarshal(bus.msgs[0])
		if ev.TraceID != "trace-1" || len(ev.IDs) != 2 || ev.Fingerprint != "ip|ua" {
			t.Errorf("event = %+v", ev)
		}
	})

	t.Run("single id, no fingerprint → no publish", func(t *testing.T) {
		bus := &capBus{}
		NewPublisher(bus, quietLog()).Publish("t", []Signal{sig("a", "x")}, "")
		if len(bus.msgs) != 0 {
			t.Errorf("published %d, want 0", len(bus.msgs))
		}
	})

	t.Run("single id WITH fingerprint publishes (for probabilistic)", func(t *testing.T) {
		bus := &capBus{}
		NewPublisher(bus, quietLog()).Publish("t", []Signal{sig("a", "x")}, "ip|ua")
		if len(bus.msgs) != 1 {
			t.Errorf("published %d, want 1", len(bus.msgs))
		}
	})

	t.Run("nil bus → nil publisher → no-op", func(t *testing.T) {
		if NewPublisher(nil, quietLog()) != nil {
			t.Error("expected nil publisher for nil bus")
		}
		var np *Publisher
		np.Publish("t", []Signal{sig("a", "x"), sig("b", "y")}, "") // must not panic
		np.PublishRequest("t", &openrtb.BidRequest{})
	})

	t.Run("PublishRequest extracts + publishes", func(t *testing.T) {
		bus := &capBus{}
		req := &openrtb.BidRequest{
			User:   &openrtb.User{ID: "u1", EIDs: []openrtb.EID{openrtb.UID2EID("uid2")}},
			Device: &openrtb.Device{IP: "1.1.1.1", UA: "UA"},
		}
		NewPublisher(bus, quietLog()).PublishRequest("t", req)
		if len(bus.msgs) != 1 {
			t.Fatalf("published %d, want 1", len(bus.msgs))
		}
		ev, _ := Unmarshal(bus.msgs[0])
		if len(ev.IDs) != 2 || ev.Fingerprint != "1.1.1.1|UA" {
			t.Errorf("event = %+v", ev)
		}
	})
}

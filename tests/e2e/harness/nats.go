//go:build e2e

package harness

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// PublishInvalidate fires a cache-invalidate message on the given NATS
// subject and returns once the broker has acked it. Used to test the
// production code path where Gateway writes trigger an invalidate publish
// and warm caches across pods reload within ~1s.
//
// Distinct from RefreshAllCaches: the refresh endpoint is the synchronous
// HTTP path tests use to skip waits; PublishInvalidate exercises the async
// pub/sub path real writes will use. Tests should assert reach with WaitFor
// since the cache reload happens off the publish path.
func (h *Harness) PublishInvalidate(t *testing.T, subject string) {
	t.Helper()
	nc, err := nats.Connect(h.URLs.NATSURL, nats.Timeout(2*time.Second))
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()

	if err := nc.Publish(subject, []byte(`{"source":"e2e-harness"}`)); err != nil {
		t.Fatalf("nats publish %s: %v", subject, err)
	}
	if err := nc.FlushTimeout(2 * time.Second); err != nil {
		t.Fatalf("nats flush: %v", err)
	}
}

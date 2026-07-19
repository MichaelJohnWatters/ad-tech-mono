// Package podid derives process/replica identifiers for NATS consumer groups.
package podid

import (
	"fmt"
	"os"
)

// Replica returns an identifier UNIQUE to this pod/replica, for use as the
// consumer-group suffix on a NATS invalidate subscription that must BROADCAST
// (fan out to every replica of a service), e.g. warm-cache and config
// invalidates.
//
// It uses the hostname: in Kubernetes the container hostname is the pod name,
// which is unique per replica. It deliberately does NOT use POD_NAME, because
// POD_NAME is pinned to a stable, SHARED value per service (e.g. "ssp-0") so
// the config system has a per-pod identity that survives restarts — that
// sharing is correct for config, but using it as a consumer-group suffix would
// collapse every replica into ONE NATS queue group, so only one replica would
// receive each invalidate (the others going stale until the next poll). Falls
// back to POD_NAME then the PID for non-container / dev environments.
func Replica() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	if n := os.Getenv("POD_NAME"); n != "" {
		return n
	}
	return fmt.Sprintf("pid-%d", os.Getpid())
}

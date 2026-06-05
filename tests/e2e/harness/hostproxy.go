//go:build e2e

package harness

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
)

// HostReachableServer wraps httptest.NewServer for tests that need the
// returned URL to be callable from inside a k8s pod, not just from the
// host process.
//
// Background: many e2e tests stand up a fake DSP / fake Prebid Server
// / fake outbound endpoint via httptest, then write the server's URL
// into a service config key (e.g. exchange.dsp_endpoints, pubad's
// outbound_prebid_url). The platform service then calls back to that
// URL. When services run as host processes (DEV_MODE=fast), the
// default httptest URL (http://127.0.0.1:NNNN) works. When services
// run as k8s pods (DEV_MODE=container), 127.0.0.1 from inside the pod
// refers to the pod's own loopback — the fake server is unreachable.
//
// This helper detects the runtime via the ADTECH_HOST_IP env var:
//
//   - ADTECH_HOST_IP set → bind the listener to 0.0.0.0 (any
//     interface), return a URL using that IP. Pods reach the host
//     via this IP. Colima's host-from-VM IP is 192.168.5.2 by default;
//     other setups can override.
//
//   - ADTECH_HOST_IP unset → behaves exactly like httptest.NewServer
//     (binds 127.0.0.1, returns 127.0.0.1:NNNN). Host-process mode.
//
// Tests use it as a drop-in replacement:
//
//   ts := harness.HostReachableServer(handler)
//   defer ts.Close()
//   ssp.SetConfigForPod(t, "exchange.dsp_endpoints", ts.URL)
//
// Pod-mode deployments also need a hostAliases entry mapping
// host.docker.internal → ADTECH_HOST_IP so test URLs that reference
// the well-known hostname keep working. See k8s/base/*/deployment.yaml.
func HostReachableServer(handler http.Handler) *httptest.Server {
	hostIP := os.Getenv("ADTECH_HOST_IP")
	if hostIP == "" {
		// Host-process mode — exactly like httptest.NewServer.
		return httptest.NewServer(handler)
	}

	// Pod mode — bind to all interfaces so the kernel routes inbound
	// pod-originated connections; substitute the public host IP into
	// the returned URL.
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		// Last-ditch fallback: 127.0.0.1. Caller will see test
		// failures if the service really is in a pod, but at least
		// host-mode tests keep passing.
		return httptest.NewServer(handler)
	}
	ts := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: handler},
	}
	ts.Start()
	// httptest.Server.URL after Start() will use whatever the kernel
	// reported as the bound address: "0.0.0.0:NNNN" on IPv4-only hosts,
	// "[::]:NNNN" on macOS where TCP listeners are dual-stack IPv6 by
	// default. Both mean "any interface" — patch either to the public
	// host IP so the URL is callable from inside the cluster.
	ts.URL = strings.Replace(ts.URL, "0.0.0.0", hostIP, 1)
	ts.URL = strings.Replace(ts.URL, "[::]", hostIP, 1)
	return ts
}

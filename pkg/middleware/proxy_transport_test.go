package middleware

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// Proxied requests must reuse upstream connections: 50 concurrent requests ×4
// rounds through ReverseProxy should open ~50 upstream conns, not ~200 (the
// DefaultTransport 2-idle-per-host behaviour that re-dialed per request).
func TestReverseProxy_ReusesUpstreamConnections(t *testing.T) {
	var newConns atomic.Int64
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	up.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			newConns.Add(1)
		}
	}
	up.Start()
	defer up.Close()

	gw := httptest.NewServer(ReverseProxy(up.URL, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer gw.Close()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}

	const conc, rounds = 50, 4
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		for i := 0; i < conc; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := client.Get(gw.URL + "/x")
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}()
		}
		wg.Wait()
	}
	if n := newConns.Load(); n > conc {
		t.Fatalf("upstream opened %d conns for %d proxied requests at concurrency %d — not reused", n, conc*rounds, conc)
	}
}

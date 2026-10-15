package httpx

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// After tuning, http.DefaultClient reuses connections under concurrency:
// 50 concurrent × 4 rounds opens ~50 upstream conns, not ~200.
func TestTuneDefaultTransport_DefaultClientReuses(t *testing.T) {
	TuneDefaultTransport()
	var newConns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	const conc, rounds = 50, 4
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		for i := 0; i < conc; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := http.DefaultClient.Get(srv.URL)
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
		t.Fatalf("DefaultClient opened %d conns for %d requests at concurrency %d — pool not tuned", n, conc*rounds, conc)
	}
}

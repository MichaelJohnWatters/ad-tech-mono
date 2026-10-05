package main

import (
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

// beaconRecorder captures every beacon the simulated player fires.
type beaconRecorder struct {
	mu   sync.Mutex
	urls []string
}

func (b *beaconRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.urls = append(b.urls, r.URL.String())
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

func (b *beaconRecorder) count(substr string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, u := range b.urls {
		if strings.Contains(u, substr) {
			n++
		}
	}
	return n
}

func wrapperDoc(t *testing.T, tagURL, beaconBase string, follow bool) []byte {
	t.Helper()
	followP := follow
	xmlBytes, err := vast.BuildWrapperAd(vast.WrapperSpec{
		AdID: "w", VASTAdTagURI: tagURL,
		FollowAdditionalWrappers: &followP,
		Trackers: vast.LinearTrackers{
			Impression: []string{beaconBase + "/b?k=wrap-imp"},
			Start:      []string{beaconBase + "/b?k=wrap-start"},
		},
		ErrorURLs: []string{beaconBase + "/b?k=wrap-err&ec=[ERRORCODE]"},
	})
	if err != nil {
		t.Fatalf("build wrapper: %v", err)
	}
	return xmlBytes
}

func inlineDoc(t *testing.T, beaconBase string) []byte {
	t.Helper()
	xmlBytes, err := vast.BuildLinearAd(vast.LinearSpec{
		AdID: "in", AdTitle: "x", Duration: 8 * time.Second,
		MediaFiles: []vast.MediaFile{{Delivery: "progressive", Type: "video/mp4", URI: "http://cdn/x.mp4"}},
		Trackers: vast.LinearTrackers{
			Impression: []string{beaconBase + "/b?k=in-imp"},
			Start:      []string{beaconBase + "/b?k=in-start"},
			Complete:   []string{beaconBase + "/b?k=in-complete"},
		},
	})
	if err != nil {
		t.Fatalf("build inline: %v", err)
	}
	return xmlBytes
}

// TestPlayVASTAd_UnwrapsChain: wrapper → inline chain fires the UNION of
// trackers (wrapper imp + start alongside the inline set).
func TestPlayVASTAd_UnwrapsChain(t *testing.T) {
	rec := &beaconRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/b", rec.handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/inline", func(w http.ResponseWriter, r *http.Request) { w.Write(inlineDoc(t, srv.URL)) })
	mux.HandleFunc("/wrap", func(w http.ResponseWriter, r *http.Request) { w.Write(wrapperDoc(t, srv.URL+"/inline", srv.URL, true)) })

	ok, err := serveVAST(srv.Client(), srv.URL+"/wrap", "", profile{}, rand.New(rand.NewSource(1)))
	if err != nil || !ok {
		t.Fatalf("serveVAST: ok=%v err=%v", ok, err)
	}
	for _, want := range []string{"k=wrap-imp", "k=in-imp", "k=wrap-start", "k=in-start", "k=in-complete"} {
		if rec.count(want) != 1 {
			t.Errorf("beacon %s fired %d times, want 1", want, rec.count(want))
		}
	}
	if n := rec.count("k=wrap-err"); n != 0 {
		t.Errorf("no chain error expected, got %d", n)
	}
}

// TestPlayVASTAd_EmptyTerminal303: a wrapper whose tag returns an Ad-less
// VAST fires the wrapper's <Error> with code 303.
func TestPlayVASTAd_EmptyTerminal303(t *testing.T) {
	rec := &beaconRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/b", rec.handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><VAST version="4.2"></VAST>`))
	})
	mux.HandleFunc("/wrap", func(w http.ResponseWriter, r *http.Request) { w.Write(wrapperDoc(t, srv.URL+"/empty", srv.URL, true)) })

	ok, err := serveVAST(srv.Client(), srv.URL+"/wrap", "", profile{}, rand.New(rand.NewSource(1)))
	if err != nil || ok {
		t.Fatalf("empty terminal must not count as served: ok=%v err=%v", ok, err)
	}
	assertErrCode(t, rec, "303")
}

// TestPlayVASTAd_DepthCap302: a self-referencing wrapper chain stops at the
// 5-hop bound and fires 302.
func TestPlayVASTAd_DepthCap302(t *testing.T) {
	rec := &beaconRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/b", rec.handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { w.Write(wrapperDoc(t, srv.URL+"/loop", srv.URL, true)) })

	ok, _ := serveVAST(srv.Client(), srv.URL+"/loop", "", profile{}, rand.New(rand.NewSource(1)))
	if ok {
		t.Fatal("looping chain must not count as served")
	}
	assertErrCode(t, rec, "302")
}

// TestPlayVASTAd_FollowFalse300: followAdditionalWrappers=false + a next hop
// that is itself a wrapper → error 300, chain abandoned.
func TestPlayVASTAd_FollowFalse300(t *testing.T) {
	rec := &beaconRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/b", rec.handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/inline", func(w http.ResponseWriter, r *http.Request) { w.Write(inlineDoc(t, srv.URL)) })
	mux.HandleFunc("/mid", func(w http.ResponseWriter, r *http.Request) { w.Write(wrapperDoc(t, srv.URL+"/inline", srv.URL, true)) })
	mux.HandleFunc("/strict", func(w http.ResponseWriter, r *http.Request) { w.Write(wrapperDoc(t, srv.URL+"/mid", srv.URL, false)) })

	ok, _ := serveVAST(srv.Client(), srv.URL+"/strict", "", profile{}, rand.New(rand.NewSource(1)))
	if ok {
		t.Fatal("follow=false chain must not play the wrapped wrapper")
	}
	assertErrCode(t, rec, "300")
	if rec.count("k=in-imp") != 0 {
		t.Error("inline beyond the forbidden hop must not play")
	}
}

func assertErrCode(t *testing.T, rec *beaconRecorder, code string) {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, u := range rec.urls {
		if !strings.Contains(u, "k=wrap-err") {
			continue
		}
		parsed, err := url.Parse(u)
		if err == nil && parsed.Query().Get("ec") == code {
			return
		}
	}
	t.Errorf("no wrapper error beacon with ec=%s fired; beacons: %v", code, rec.urls)
}

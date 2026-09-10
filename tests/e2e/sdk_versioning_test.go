//go:build e2e

// adtech.js SDK versioning + CDN-friendly serving (PLAN Phase 11 #106): the gateway
// serves the embeddable SDK under versioned /sdk/ URLs with per-channel cache
// headers, an integrity hash, and a public version-metadata endpoint. Publishers
// pin a stable major channel (patched, short cache) or an exact version (immutable +
// SRI). Exercises the live gateway.
package e2e

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestSDKVersionedServing(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	get := func(path string, hdr map[string]string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := h.HTTP.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	// version.json — the public metadata publishers/tools read.
	resp, body := get("/sdk/version.json", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("version.json status %d", resp.StatusCode)
	}
	var meta struct {
		Version string `json:"version"`
		Major   string `json:"major"`
		Pinned  struct {
			URL       string `json:"url"`
			Integrity string `json:"integrity"`
		} `json:"pinned"`
		MajorURL  string `json:"major_url"`
		LatestURL string `json:"latest_url"`
	}
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatalf("version.json decode: %v (%s)", err, body)
	}
	if meta.Version == "" || meta.Major != "v"+strings.SplitN(meta.Version, ".", 2)[0] {
		t.Fatalf("bad version metadata: %+v", meta)
	}
	if !strings.HasPrefix(meta.Pinned.Integrity, "sha384-") {
		t.Errorf("integrity = %q, want sha384-…", meta.Pinned.Integrity)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("version.json missing ACAO:*")
	}

	// Per-channel serving + cache headers.
	for _, tc := range []struct {
		path      string
		wantCache string
	}{
		{meta.Pinned.URL, "public, max-age=31536000, immutable"},
		{meta.MajorURL, "public, max-age=3600"},
		{meta.LatestURL, "public, max-age=300"},
	} {
		resp, body := get(tc.path, nil)
		if resp.StatusCode != 200 {
			t.Errorf("%s status %d, want 200", tc.path, resp.StatusCode)
			continue
		}
		if cc := resp.Header.Get("Cache-Control"); cc != tc.wantCache {
			t.Errorf("%s Cache-Control=%q, want %q", tc.path, cc, tc.wantCache)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
			t.Errorf("%s Content-Type=%q, want application/javascript", tc.path, ct)
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s missing ACAO:*", tc.path)
		}
		if !strings.Contains(body, "SDK_VERSION") {
			t.Errorf("%s did not serve the SDK bytes", tc.path)
		}
	}

	// THE core invariant: the advertised SRI integrity must equal sha384 of the
	// exact bytes served at the pinned URL — else a publisher pinning it gets a
	// browser SRI mismatch and the SDK silently fails to load.
	_, pinnedBody := get(meta.Pinned.URL, nil)
	sum := sha512.Sum384([]byte(pinnedBody))
	want := "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
	if meta.Pinned.Integrity != want {
		t.Errorf("advertised integrity %q != sha384 of served bytes %q", meta.Pinned.Integrity, want)
	}

	// A version we don't host → 404 (never silently serve the current build).
	if resp, _ := get("/sdk/v1/adtech.js", nil); resp.StatusCode != 404 {
		t.Errorf("unknown version: status %d, want 404", resp.StatusCode)
	}

	// Conditional GET on the immutable pinned URL → 304.
	etagResp, _ := get(meta.Pinned.URL, nil)
	etag := etagResp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("pinned URL missing ETag")
	}
	if resp, _ := get(meta.Pinned.URL, map[string]string{"If-None-Match": etag}); resp.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match: got %d, want 304", resp.StatusCode)
	}
}

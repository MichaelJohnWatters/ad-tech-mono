package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/sdkasset"
)

// sdkHandler serves the embeddable adtech.js SDK under versioned, cache-friendly
// URLs (PLAN #106), mounted at /sdk/. Public (no auth — publishers load it cross-
// origin from their pages):
//
//	GET /sdk/<exact>/adtech.js  → immutable 1y  (e.g. /sdk/2.0.0/adtech.js)
//	GET /sdk/v<major>/adtech.js → 1h, patched   (e.g. /sdk/v2/adtech.js)
//	GET /sdk/latest/adtech.js   → short, floats
//	GET /sdk/version.json       → { version, major, integrity, urls{…} }
//
// A version segment we don't host → 404 (never silently serve the current build for
// a mismatched pin). All responses carry ACAO:* so SRI/crossorigin fetches work.
func sdkHandler(a *sdkasset.Asset, log *slog.Logger) http.HandlerFunc {
	metadata, _ := json.Marshal(struct {
		Version   string `json:"version"`
		Major     string `json:"major"`
		Integrity string `json:"integrity"`
		URLs      struct {
			Pinned string `json:"pinned"`
			Major  string `json:"major"`
			Latest string `json:"latest"`
		} `json:"urls"`
	}{
		Version: a.Version, Major: a.Major, Integrity: a.Integrity,
		URLs: struct {
			Pinned string `json:"pinned"`
			Major  string `json:"major"`
			Latest string `json:"latest"`
		}{Pinned: a.PinnedURL(), Major: a.MajorURL(), Latest: a.LatestURL()},
	})

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		rest := strings.TrimPrefix(r.URL.Path, "/sdk/")

		if rest == "version.json" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", sdkasset.CacheControl(sdkasset.ChannelLatest))
			if r.Method == http.MethodHead {
				return
			}
			w.Write(metadata)
			return
		}

		// Expect "<version>/adtech.js".
		seg, file, ok := strings.Cut(rest, "/")
		if !ok || file != "adtech.js" {
			http.NotFound(w, r)
			return
		}
		ch, hosted := a.Resolve(seg)
		if !hosted {
			http.Error(w, "unknown sdk version", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", sdkasset.CacheControl(ch))
		w.Header().Set("ETag", a.ETag)
		if inm := r.Header.Get("If-None-Match"); inm != "" && inm == a.ETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.Method == http.MethodHead {
			return
		}
		w.Write(a.Bytes)
	}
}

package main

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

// uidCookieName is the cookie our bidder sets on the user's browser, keyed
// to our domain. Mirrors the per-bidder cookie pattern Prebid uses (each
// bidder gets its own cookie under its own domain after the user-sync flow).
const uidCookieName = "_adtm_uid"

// uidCookieMaxAge is the TTL on the persisted UID cookie. 30 days matches
// the typical IAB user-sync convention; longer than 30 days drifts past
// consent recency thresholds in some regions.
const uidCookieMaxAge = 30 * 24 * time.Hour

// prebidSetUIDHandler implements the Prebid bidder cookie-sync endpoint.
// Called by external Prebid Servers via the user-sync iframe/pixel: they
// hand us a publisher-side user identifier, we record our own ID for the
// user (or mint one), and return a transparent 1x1 GIF or redirect.
//
// Query params:
//
//	uid       — our internal user ID, if the Prebid Server already has one
//	            for this bidder. If empty, we mint a new ID and set it.
//	bidder    — bidder code; expected to be "adtechmono". Validated.
//	gdpr      — GDPR-applies flag (0/1). Logged.
//	gdpr_consent — TCF consent string. Logged.
//	f         — response format: "i" (image, default) or "b" (blank/no body).
//
// MVP scope: we set our cookie and log the inbound mapping. Storing the
// publisher-side → internal-ID link in a shared store (Redis/Postgres)
// requires architecture work that's deferred until we have a real Prebid
// partner. The cookie alone is enough for our exchange to recognise repeat
// users on subsequent /v1/prebid/openrtb2/auction calls (browser sends it
// automatically when same-origin).
func prebidSetUIDHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bidder := r.URL.Query().Get("bidder")
		if bidder != "" && bidder != "adtechmono" {
			http.Error(w, "unknown bidder", http.StatusBadRequest)
			return
		}

		inboundUID := r.URL.Query().Get("uid")
		ourID := inboundUID
		if ourID == "" {
			// Reuse existing cookie if present so repeat sync calls don't
			// churn the user's ID on every visit.
			if c, err := r.Cookie(uidCookieName); err == nil {
				ourID = c.Value
			}
		}
		if ourID == "" {
			ourID = mintUserID()
		}

		http.SetCookie(w, &http.Cookie{
			Name:     uidCookieName,
			Value:    ourID,
			Path:     "/",
			MaxAge:   int(uidCookieMaxAge.Seconds()),
			SameSite: http.SameSiteNoneMode,
			Secure:   r.TLS != nil,
			HttpOnly: true,
		})

		reqLog := logger.WithContext(log, r.Context())
		reqLog.Info("prebid setuid",
			"bidder", "adtechmono",
			"inbound_uid", inboundUID,
			"our_uid", ourID,
			"gdpr", r.URL.Query().Get("gdpr"),
		)

		switch r.URL.Query().Get("f") {
		case "b":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set(constants.HeaderContentType, "image/gif")
			w.Write(transparentGIF)
		}
	}
}

// transparentGIF is a 1x1 transparent pixel — what Prebid bidders return
// from setuid by default.
var transparentGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00,
	0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0x21,
	0xf9, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00, 0x2c, 0x00, 0x00,
	0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02, 0x44,
	0x01, 0x00, 0x3b,
}

func mintUserID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "adtm-" + hex.EncodeToString(b)
}

package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adcert"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// AdCertKeyResponse is the JSON the exchange publishes at /v1/adcert/key so
// DSPs can fetch the verification key (and pick up rotations).
type AdCertKeyResponse struct {
	Alg string `json:"alg"` // "Ed25519"
	Key string `json:"key"` // base64 raw-url public key
}

// adCertKeyHandler serves the exchange's ads.cert public key derived from the
// configured signing key. Returns 404 when signing isn't configured, so the
// endpoint is only "live" when there's actually a key to verify against.
func adCertKeyHandler(cfg *config.Config, log *slog.Logger) http.HandlerFunc {
	// Derived once at boot: the sign key is TierSecret (env-only), so it can't
	// change without a restart anyway.
	priv, err := adcert.ParsePrivateKey(cfg.Get("exchange.adcert_sign_key", ""))
	if err != nil {
		log.Error("adcert: invalid signing key, key endpoint disabled", "error", err)
	}
	pubB64 := adcert.PublicKeyB64(priv)
	return func(w http.ResponseWriter, r *http.Request) {
		if pubB64 == "" {
			http.Error(w, "adcert signing not configured", http.StatusNotFound)
			return
		}
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		// Public key — safe to cache briefly; rotations are infrequent.
		w.Header().Set("Cache-Control", "public, max-age=60")
		json.NewEncoder(w).Encode(AdCertKeyResponse{Alg: "Ed25519", Key: pubB64})
	}
}

// adCertSignerFn returns a function that signs an outbound bid request with the
// exchange's ads.cert Ed25519 key, stamping the signature into
// Source.Ext.AdCert. When no key is configured (or it's invalid) it returns a
// no-op so signing simply stays off — matching the fail-open posture of the
// other trust gates. The key is TierSecret, read once at boot.
func adCertSignerFn(cfg *config.Config, log *slog.Logger, now func() time.Time) func(*openrtb.BidRequest) {
	priv, err := adcert.ParsePrivateKey(cfg.Get("exchange.adcert_sign_key", ""))
	if err != nil {
		log.Error("adcert: invalid signing key, request signing disabled", "error", err)
		return func(*openrtb.BidRequest) {}
	}
	if priv == nil {
		return func(*openrtb.BidRequest) {} // not configured
	}
	log.Info("adcert: signing outbound bid requests")
	return func(req *openrtb.BidRequest) {
		if req.Source == nil {
			req.Source = &openrtb.Source{}
		}
		if req.Source.Ext == nil {
			req.Source.Ext = &openrtb.SourceExt{}
		}
		// Stamp the signing time first — it's part of the canonical, so the
		// signature covers it and the DSP can reject stale replays.
		req.Source.Ext.AdCertTS = now().Unix()
		req.Source.Ext.AdCert = adcert.Sign(priv, req)
	}
}

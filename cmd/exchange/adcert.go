package main

import (
	"log/slog"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adcert"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// adCertSignerFn returns a function that signs an outbound bid request with the
// exchange's ads.cert Ed25519 key, stamping the signature into
// Source.Ext.AdCert. When no key is configured (or it's invalid) it returns a
// no-op so signing simply stays off — matching the fail-open posture of the
// other trust gates. The key is TierSecret, read once at boot.
func adCertSignerFn(cfg *config.Config, log *slog.Logger) func(*openrtb.BidRequest) {
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
		req.Source.Ext.AdCert = adcert.Sign(priv, req)
	}
}

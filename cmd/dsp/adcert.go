package main

import (
	"log/slog"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adcert"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// adCertVerifierFn returns a per-request check on the ads.cert signature of an
// inbound bid request, mirroring the exchange's schain gate. The verify key is
// TierStatic (read once at boot); the enforcement mode is read live so ops can
// ratchet off → warn → strict without a restart:
//
//   - off:    always allow, no check.
//   - warn:   verify; on a missing/invalid signature log and bid anyway.
//   - strict: verify; on a missing/invalid signature no-bid.
//
// Verification is also skipped (allow) when no key is configured, so the
// feature stays inert until the exchange is actually signing.
func adCertVerifierFn(cfg *config.Config, log *slog.Logger, now func() time.Time) func(*openrtb.BidRequest) (bool, string) {
	pub, err := adcert.ParsePublicKey(cfg.Get("dsp.adcert_verify_key", ""))
	if err != nil {
		log.Error("adcert: invalid verify key, verification disabled", "error", err)
		pub = nil
	}
	return func(req *openrtb.BidRequest) (bool, string) {
		mode := strings.ToLower(strings.TrimSpace(cfg.Get("dsp.adcert_enforcement", "off")))
		if mode == "" || mode == "off" || pub == nil {
			return true, ""
		}
		var sig string
		if req.Source != nil && req.Source.Ext != nil {
			sig = req.Source.Ext.AdCert
		}
		// Both must hold: a valid signature AND a fresh timestamp (replay
		// protection). maxAge <= 0 disables the freshness check.
		maxAge := cfg.GetDuration("dsp.adcert_max_age", 5*time.Minute)
		sigOK := adcert.Verify(pub, req, sig)
		fresh := adcert.Fresh(req, now(), maxAge)
		if sigOK && fresh {
			return true, ""
		}
		reason := "adcert_invalid"
		if sigOK && !fresh {
			reason = "adcert_stale"
		}
		if mode == "strict" {
			return false, reason
		}
		log.Warn("adcert check failed (warn mode, bidding anyway)", "reason", reason, "trace_id", req.ID)
		return true, ""
	}
}

package main

import (
	"log/slog"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// schainGateFn returns a per-auction check on the inbound SupplyChain (schain),
// mirroring adsTxtGateFn. It reads exchange.schain_enforcement live so ops can
// ratchet without a restart:
//
//   - off:    always allow, never touch the request.
//   - warn:   validate; on a missing/malformed schain log and allow.
//   - strict: validate; on a missing/malformed schain reject (no-bid).
//
// When exchange.schain_append_node is on it appends this exchange as a further
// node (only correct when the exchange is a distinct reseller from the SSP —
// off by default so the single-platform chain the SSP emits isn't inflated).
// The returned func mutates req in place when appending.
func schainGateFn(cfg *config.Config, log *slog.Logger) func(req *openrtb.BidRequest, traceID string) (bool, string) {
	return func(req *openrtb.BidRequest, traceID string) (bool, string) {
		mode := strings.ToLower(strings.TrimSpace(cfg.Get("exchange.schain_enforcement", "warn")))
		if mode == "" || mode == "off" {
			return true, ""
		}

		if err := openrtb.ValidateSChain(openrtb.SChainOf(req)); err != nil {
			if mode == "strict" {
				return false, "schain_invalid"
			}
			// warn: surface it but let the auction proceed.
			log.Warn("schain validation failed (warn mode, allowing)", "error", err, "trace_id", traceID)
			return true, ""
		}

		if cfg.GetBool("exchange.schain_append_node", false) {
			appendExchangeNode(cfg, req, traceID)
		}
		return true, ""
	}
}

// appendExchangeNode adds this exchange as a downstream schain node. Assumes the
// schain is already valid (the caller validated it). Uses the exchange's ads.txt
// seller identity as its asi/sid so the appended node is consistent with what
// the exchange advertises elsewhere.
func appendExchangeNode(cfg *config.Config, req *openrtb.BidRequest, traceID string) {
	sc := openrtb.SChainOf(req)
	if sc == nil {
		return
	}
	sc.Nodes = append(sc.Nodes, openrtb.SupplyChainNode{
		ASI: cfg.Get("exchange.adstxt_seller_domain", ""),
		SID: cfg.Get("exchange.adstxt_seller_id", ""),
		RID: traceID,
		HP:  1,
	})
}

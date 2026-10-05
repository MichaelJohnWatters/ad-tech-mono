package main

import (
	"log/slog"
	"net/http"
	"net/url"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// attachNoticeURLs sets the buyer-supplied win/loss/billing notice URLs
// (OpenRTB §4.4 nurl/lurl/burl) on the final winning candidate. The URLs
// point back at THIS DSP's own notice endpoints with the standard auction
// macros; the SELLER side substitutes and fires them — the query params are
// deliberately the same names the legacy fixed-endpoint notices carried, so
// winHandler/lossHandler consume either contract unchanged.
//
// noticeBase is the HTTP base other cluster services can reach this DSP on
// (DSP_NOTICE_BASE_URL, e.g. http://dsp-internal:8082). Empty = emit no
// notice URLs; the exchange falls back to the legacy ;notify= convention —
// the safe default for host-run dev and not-yet-configured competitor pods.
//
// Pure string building on the final bestBid only (hot-path iron rule).
func attachNoticeURLs(bid *openrtb.BidObj, noticeBase, campaignID, placementID string) {
	if noticeBase == "" {
		return
	}
	q := "bid_id=${AUCTION_BID_ID}&campaign_id=" + url.QueryEscape(campaignID) +
		"&placement_id=" + url.QueryEscape(placementID)
	bid.NURL = noticeBase + routes.OpenRTBWin + "?" + q +
		"&price=${AUCTION_PRICE}&clear_price=${AUCTION_MIN_TO_WIN}"
	bid.LURL = noticeBase + routes.OpenRTBLoss + "?" + q +
		"&reason=${AUCTION_LOSS}&clearing_price=${AUCTION_PRICE}"
	bid.BURL = noticeBase + routes.OpenRTBBilling + "?" + q + "&price=${AUCTION_PRICE}"
}

// billingHandler receives the burl billing notice — the seller's signal that
// the impression actually BOOKED (distinct from the win notice, which fires
// at auction time whether or not the ad ever renders). The platform's own
// money truth is the AuctionWinEvent + billing engine, so internally this is
// observability: log it; the standard slot exists so external sellers can
// bill this DSP the standard way.
func billingHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		log.Debug("billing notice (burl) received",
			"bid_id", q.Get("bid_id"),
			"campaign_id", q.Get("campaign_id"),
			"placement_id", q.Get("placement_id"),
			"price", q.Get("price"))
		w.WriteHeader(http.StatusNoContent)
	}
}

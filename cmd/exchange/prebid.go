package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identityobserve"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/prebid"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// prebidAuctionHandler is the inbound Prebid bidder endpoint. Wire format
// is OpenRTB 2.x (Prebid speaks it natively), so the only translation needed
// is the floor policy + opaque deal-id logging. Once those are applied we
// reuse the existing auction path verbatim by invoking auctionHandler with
// the mutated request.
//
// Loopback note: rather than call the http handler from within a handler
// (which would require synthesizing a *http.Request + httptest recorder),
// we extract the request body and dispatch via the standard auctionHandler
// closure. The auctionHandler is itself an http.HandlerFunc, so we adapt by
// constructing a new in-memory request with the mutated body and a fresh
// ResponseRecorder that buffers the auction's output before we copy it to
// the real ResponseWriter. This keeps both endpoints in lockstep without
// duplicating auction logic.
func prebidAuctionHandler(cfg *config.Config, auction http.HandlerFunc, idPub *identityobserve.Publisher, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !keys.Exchange.PrebidEnabled.Get(cfg) {
			http.Error(w, "prebid endpoint disabled", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}

		var bidReq openrtb.BidRequest
		if err := json.Unmarshal(body, &bidReq); err != nil {
			http.Error(w, "invalid bid request", http.StatusBadRequest)
			return
		}

		minFloor := keys.Exchange.PrebidMinBidFloor.Get(cfg)
		reqLog := logger.WithContext(log, r.Context())
		raised := prebid.ApplyFloorPolicy(&bidReq, minFloor, reqLog)

		// Identity auto-build: external Prebid demand carries user/device
		// identifiers our own SSP never saw — feed them to the identity graph.
		// No-op when observation is disabled (nil publisher).
		idPub.PublishRequest(tracing.TraceIDFromContext(r.Context()), &bidReq)

		reqLog.Info("prebid inbound auction",
			"bidder_code", prebid.BidderCode,
			"imps", len(bidReq.Imp),
			"floors_raised", raised,
			"min_floor", minFloor,
		)

		// Re-encode the (possibly mutated) request and dispatch through the
		// shared auctionHandler. We pass through the original trace context
		// so the auction span chains correctly under the inbound Prebid
		// request span.
		mutated, _ := json.Marshal(bidReq)
		inner := r.Clone(r.Context())
		inner.Body = io.NopCloser(bytes.NewReader(mutated))
		inner.ContentLength = int64(len(mutated))
		inner.Method = http.MethodPost
		inner.Header.Set(constants.HeaderContentType, constants.ContentTypeJSON)
		// Preserve traceparent so the auction span chains under this one
		// (HTTPMiddleware already populated r.Context() with the inbound span).
		tracing.InjectHTTP(r.Context(), inner)

		auction(w, inner)
	}
}

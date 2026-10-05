package openrtb

import (
	"fmt"
	"strings"
)

// ExpandAuctionMacros performs the OpenRTB 2.6 §4.4 win-time substitution on
// a winner's ad markup: the SELLER side (exchange, or the publisher-adserver
// on its direct Prebid fan-out, which bypasses the exchange) knows the final
// clearing price, so ${AUCTION_PRICE} / ${AUCTION_ID} / ${AUCTION_CURRENCY}
// in adm are replaced when the winner is assembled. price is ALWAYS the
// clearing price the buyer pays (post-shading / per-slot for multi-winner)
// — substituting the raw bid would leak the buyer's valuation and
// over-report spend. No-op (allocation-free) when the markup carries no
// ${ macros.
func ExpandAuctionMacros(adm string, price float64, auctionID, currency string) string {
	if adm == "" || !strings.Contains(adm, "${") {
		return adm
	}
	return strings.NewReplacer(
		"${AUCTION_PRICE}", fmt.Sprintf("%.4f", price),
		"${AUCTION_ID}", auctionID,
		"${AUCTION_CURRENCY}", currency,
	).Replace(adm)
}

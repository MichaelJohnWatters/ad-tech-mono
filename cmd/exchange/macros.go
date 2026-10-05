package main

import (
	"fmt"
	"strings"
)

// expandAuctionMacros performs the OpenRTB 2.6 §4.4 win-time substitution on
// the winner's ad markup: the exchange (not the buyer) knows the final
// clearing price, so ${AUCTION_PRICE} / ${AUCTION_ID} / ${AUCTION_CURRENCY}
// in adm are replaced when the winner is assembled. price is ALWAYS the
// clearing price (post-shading / per-slot for multi-winner) — substituting
// the raw bid would leak the buyer's valuation and over-report spend.
// No-op (allocation-free) when the markup carries no ${ macros.
func expandAuctionMacros(adm string, price float64, auctionID, currency string) string {
	if adm == "" || !strings.Contains(adm, "${") {
		return adm
	}
	return strings.NewReplacer(
		"${AUCTION_PRICE}", fmt.Sprintf("%.4f", price),
		"${AUCTION_ID}", auctionID,
		"${AUCTION_CURRENCY}", currency,
	).Replace(adm)
}

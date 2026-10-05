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

// NoticeMacros carries the win/loss/billing-notice substitution values for
// buyer-supplied nurl/lurl/burl (OpenRTB 2.6 §4.4 substitution macros).
type NoticeMacros struct {
	Price      float64 // ${AUCTION_PRICE} — the clearing price (what the winner pays / the price that beat a loser)
	MinToWin   float64 // ${AUCTION_MIN_TO_WIN} — minimum bid that would have won (bid-shading feedback)
	AuctionID  string  // ${AUCTION_ID}
	BidID      string  // ${AUCTION_BID_ID} — the buyer's bid.id, echoed back
	ImpID      string  // ${AUCTION_IMP_ID}
	Currency   string  // ${AUCTION_CURRENCY}
	LossReason int     // ${AUCTION_LOSS} — OpenRTB loss-reason code (lurl only)
}

// ExpandNoticeMacros performs the §4.4 substitution on a buyer-supplied
// notice URL (nurl/lurl/burl). The SELLER side substitutes and fires these —
// the buyer baked the macros into its bid and must never see them literal.
// No-op (allocation-free) when the URL carries no ${ macros.
func ExpandNoticeMacros(u string, m NoticeMacros) string {
	if u == "" || !strings.Contains(u, "${") {
		return u
	}
	return strings.NewReplacer(
		"${AUCTION_PRICE}", fmt.Sprintf("%.4f", m.Price),
		"${AUCTION_MIN_TO_WIN}", fmt.Sprintf("%.4f", m.MinToWin),
		"${AUCTION_ID}", m.AuctionID,
		"${AUCTION_BID_ID}", m.BidID,
		"${AUCTION_IMP_ID}", m.ImpID,
		"${AUCTION_CURRENCY}", m.Currency,
		"${AUCTION_LOSS}", fmt.Sprintf("%d", m.LossReason),
	).Replace(u)
}

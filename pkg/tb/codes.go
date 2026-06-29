package tb

// TB transfer `code` enum. TigerBeetle treats `code` as opaque metadata —
// no enforcement, no querying — but using a stable set makes ledger dumps
// readable and lets reconciliation tools group transfers by purpose.
const (
	CodeSpend       uint16 = 1 // CPM immediate billing (advertiser → publisher)
	CodeReservation uint16 = 2 // Pending hold (advertiser → escrow, two-phase pending)
	CodeSettlement  uint16 = 3 // Reservation fulfilled (publisher slice on settle)
	CodeRelease     uint16 = 4 // Reservation released (void-pending on expiry/explicit)
	CodeMargin      uint16 = 5 // Platform margin slice (advertiser → house)
)

// TB account `code` enum. Mirrors the EntryType taxonomy but at the account
// level: every TB account has a single code that describes its role.
const (
	AccountCodeAdvertiser uint16 = 10
	AccountCodePublisher  uint16 = 11
	AccountCodeEscrow     uint16 = 12
	AccountCodeHouse      uint16 = 13
)

// Bid model codes packed into transfer.user_data_32. The low byte holds
// the bid model, freeing the upper 24 bits for future packing (e.g. deal
// type). Encoded this way so a single uint32 read from TB tells the
// reconciler which billing rule produced the transfer.
const (
	BidModelUnknown uint8 = 0
	BidModelCPM     uint8 = 1
	BidModelCPC     uint8 = 2
	BidModelCPA     uint8 = 3
	BidModelVCPM    uint8 = 4
	BidModelCPCV    uint8 = 5
)

// PackBidModel encodes a bid model string into the low byte of a uint32
// suitable for TB transfer.user_data_32. Unknown models pack as zero
// (BidModelUnknown) so a typo doesn't silently land in an unrelated bucket.
func PackBidModel(model string) uint32 {
	return uint32(bidModelCode(model))
}

// UnpackBidModel is the inverse of PackBidModel.
func UnpackBidModel(packed uint32) string {
	switch uint8(packed & 0xff) {
	case BidModelCPM:
		return "cpm"
	case BidModelCPC:
		return "cpc"
	case BidModelCPA:
		return "cpa"
	case BidModelVCPM:
		return "vcpm"
	case BidModelCPCV:
		return "cpcv"
	}
	return ""
}

func bidModelCode(model string) uint8 {
	switch model {
	case "cpm":
		return BidModelCPM
	case "cpc":
		return BidModelCPC
	case "cpa":
		return BidModelCPA
	case "vcpm":
		return BidModelVCPM
	case "cpcv":
		return BidModelCPCV
	}
	return BidModelUnknown
}

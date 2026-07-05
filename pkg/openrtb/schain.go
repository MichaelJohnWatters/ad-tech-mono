package openrtb

import (
	"errors"
	"fmt"
)

// SChainVersion is the IAB SupplyChain object spec version we emit.
const SChainVersion = "1.0"

// Errors returned by ValidateSChain. The exchange maps these to its
// schain_enforcement policy (strict rejects, warn logs, off ignores).
var (
	ErrSChainMissing    = errors.New("schain: supply chain object missing")
	ErrSChainNoNodes    = errors.New("schain: no nodes")
	ErrSChainBadVersion = errors.New("schain: missing ver")
)

// ValidateSChain checks that a SupplyChain is well-formed per the IAB spec:
// a version is set, at least one node is present, and every node carries the
// required asi + sid with hp in {0,1}. It does not verify the chain against
// ads.txt/sellers.json (that is a separate authorisation step) — it only
// asserts structural validity so the exchange can reject garbage. Returns nil
// for a valid chain.
func ValidateSChain(sc *SupplyChain) error {
	if sc == nil {
		return ErrSChainMissing
	}
	if sc.Ver == "" {
		return ErrSChainBadVersion
	}
	if len(sc.Nodes) == 0 {
		return ErrSChainNoNodes
	}
	for i, n := range sc.Nodes {
		if n.ASI == "" {
			return fmt.Errorf("schain: node %d missing asi", i)
		}
		if n.SID == "" {
			return fmt.Errorf("schain: node %d missing sid", i)
		}
		if n.HP != 0 && n.HP != 1 {
			return fmt.Errorf("schain: node %d hp must be 0 or 1, got %d", i, n.HP)
		}
	}
	return nil
}

// SChainOf safely extracts the SupplyChain from a bid request, returning nil
// when any link in source.ext.schain is absent.
func SChainOf(req *BidRequest) *SupplyChain {
	if req == nil || req.Source == nil || req.Source.Ext == nil {
		return nil
	}
	return req.Source.Ext.SChain
}

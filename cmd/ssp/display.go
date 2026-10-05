package main

import (
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// displayAdMForServe returns the display winner's bid.adm HTML when it's
// worth attempting the external-adm serve path (OpenRTB §4.3: display adm is
// HTML), else "". Defensive guard: markup starting with '{' is an OpenRTB
// Native JSON payload — native has its own serve branch, but a mislabelled
// bid must not get its JSON injected into a page as "HTML". The exchange has
// already substituted the §4.4 auction macros into winner adm, so the caller
// must NOT expand again.
func displayAdMForServe(winner openrtb.BidObj) string {
	adm := strings.TrimSpace(winner.AdM)
	if adm == "" || strings.HasPrefix(adm, "{") {
		return ""
	}
	return adm
}

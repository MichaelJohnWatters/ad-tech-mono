package main

import (
	"strings"
	"testing"
)

// TestExpandAuctionMacros: §4.4 substitution uses the CLEARING price, leaves
// macro-free markup untouched, and never leaves a literal ${AUCTION_*} behind.
func TestExpandAuctionMacros(t *testing.T) {
	adm := `<VAST version="4.2"><Ad><InLine><Impression><![CDATA[https://buyer/imp?p=${AUCTION_PRICE}&a=${AUCTION_ID}&c=${AUCTION_CURRENCY}]]></Impression></InLine></Ad></VAST>`
	out := expandAuctionMacros(adm, 4.25, "trace-1", "USD")
	for _, want := range []string{"p=4.2500", "a=trace-1", "c=USD"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(out, "${AUCTION") {
		t.Errorf("unsubstituted auction macro left behind: %s", out)
	}
	if got := expandAuctionMacros("no macros here", 1, "t", "USD"); got != "no macros here" {
		t.Errorf("macro-free markup must be untouched, got %q", got)
	}
	if got := expandAuctionMacros("", 1, "t", "USD"); got != "" {
		t.Errorf("empty adm must stay empty")
	}
}

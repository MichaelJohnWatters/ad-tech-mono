package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// TestAdmForMediaServe pins the protocol gate: a winner whose declared
// protocol is in the imp's advertised set passes its adm through; a mismatch
// DROPS the adm (MediaURL fallback serves — the settled win is never
// rejected); undeclared protocol (0, legacy) passes ungated.
func TestAdmForMediaServe(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	acFor := func(protocols []int) auctionContext {
		return auctionContext{BidReq: openrtb.BidRequest{Imp: []openrtb.Imp{{
			Video: &openrtb.Video{Protocols: protocols},
		}}}}
	}
	adm := `<VAST version="4.2"></VAST>`

	// Declared 13 (VAST 4.2), imp accepts it → adm passes.
	if got := admForMediaServe(acFor([]int{2, 3, 7, 11, 13, 14}), openrtb.BidObj{AdM: adm, Protocol: openrtb.ProtocolVAST42}, log); got != adm {
		t.Errorf("accepted protocol must pass adm through, got %q", got)
	}
	// Imp only accepts VAST 2.0 → adm dropped, not an error.
	if got := admForMediaServe(acFor([]int{2}), openrtb.BidObj{AdM: adm, Protocol: openrtb.ProtocolVAST42, MediaURL: "http://cdn/x.mp4"}, log); got != "" {
		t.Errorf("protocol mismatch must drop adm, got %q", got)
	}
	// Undeclared protocol (legacy bidder) → ungated pass-through.
	if got := admForMediaServe(acFor([]int{2}), openrtb.BidObj{AdM: adm}, log); got != adm {
		t.Errorf("protocol 0 must pass ungated, got %q", got)
	}
	// Imp advertised no protocols → ungated.
	if got := admForMediaServe(acFor(nil), openrtb.BidObj{AdM: adm, Protocol: openrtb.ProtocolVAST42}, log); got != adm {
		t.Errorf("no advertised set must pass ungated, got %q", got)
	}
	// Audio imp path.
	acAudio := auctionContext{BidReq: openrtb.BidRequest{Imp: []openrtb.Imp{{
		Audio: &openrtb.Audio{Protocols: []int{9, 10, 11, 13}},
	}}}}
	if got := admForMediaServe(acAudio, openrtb.BidObj{AdM: adm, Protocol: openrtb.ProtocolVAST42}, log); got != adm {
		t.Errorf("audio imp accepting 13 must pass adm, got %q", got)
	}
}

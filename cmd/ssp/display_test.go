package main

import (
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// TestDisplayAdMForServe: HTML markup passes through; empty and native-JSON
// payloads are rejected (native has its own serve branch — a mislabelled
// JSON bid must never be injected into a page as HTML).
func TestDisplayAdMForServe(t *testing.T) {
	html := `<div onclick="x()">EXTERNAL DSP</div>`
	if got := displayAdMForServe(openrtb.BidObj{AdM: html}); got != html {
		t.Errorf("HTML adm must pass through, got %q", got)
	}
	if got := displayAdMForServe(openrtb.BidObj{AdM: "  \n" + html}); got != html {
		t.Errorf("leading whitespace must trim, got %q", got)
	}
	if got := displayAdMForServe(openrtb.BidObj{}); got != "" {
		t.Errorf("empty adm must yield empty, got %q", got)
	}
	if got := displayAdMForServe(openrtb.BidObj{AdM: `{"native":{"ver":"1.2"}}`}); got != "" {
		t.Errorf("native JSON adm must be rejected, got %q", got)
	}
}

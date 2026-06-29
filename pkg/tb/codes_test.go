package tb

import "testing"

func TestPackUnpackBidModel(t *testing.T) {
	cases := []string{"cpm", "cpc", "cpa", "vcpm", "cpcv"}
	for _, m := range cases {
		got := UnpackBidModel(PackBidModel(m))
		if got != m {
			t.Errorf("round-trip %q -> %d -> %q", m, PackBidModel(m), got)
		}
	}
}

func TestPackBidModelUnknown(t *testing.T) {
	if got := PackBidModel("nonsense"); got != 0 {
		t.Errorf("PackBidModel(unknown) = %d, want 0", got)
	}
	if got := UnpackBidModel(0); got != "" {
		t.Errorf("UnpackBidModel(0) = %q, want \"\"", got)
	}
}

func TestBidModelOccupiesLowByteOnly(t *testing.T) {
	// Upper 24 bits are reserved. Round-trip must ignore them so a later
	// scheme that uses them for deal-type packing stays compatible.
	packed := PackBidModel("cpc") | (0xdeadbe00)
	if got := UnpackBidModel(packed); got != "cpc" {
		t.Errorf("upper-bit pollution should be ignored, got %q", got)
	}
}

func TestCodesAreDistinct(t *testing.T) {
	codes := []uint16{CodeSpend, CodeReservation, CodeSettlement, CodeRelease, CodeMargin}
	seen := make(map[uint16]bool, len(codes))
	for _, c := range codes {
		if seen[c] {
			t.Errorf("duplicate transfer code: %d", c)
		}
		seen[c] = true
	}
}

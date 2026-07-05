package privacy

import (
	"encoding/base64"
	"testing"
)

// usNat describes the three US National opt-out fields we decode, so tests can
// build a section with known values. Each is 0=N/A, 1=opted out, 2=not opted out.
type usNat struct {
	sale, sharing, targeted int
}

// bitWriter is the inverse of bitReader — it lets tests construct a section with
// the exact core-segment layout usNationalOptOut decodes, so the encode/decode
// pair pins the field offsets.
type bitWriter struct {
	bits []int
}

func (w *bitWriter) write(val, n int) {
	for i := n - 1; i >= 0; i-- {
		w.bits = append(w.bits, (val>>i)&1)
	}
}

func (w *bitWriter) bytes() []byte {
	out := make([]byte, (len(w.bits)+7)/8)
	for i, b := range w.bits {
		if b == 1 {
			out[i/8] |= 1 << (7 - (i % 8))
		}
	}
	return out
}

// encodeUSNational builds a base64url US National core segment matching the
// layout documented in gpp.go (Version + 6 notice fields + the three opt-outs).
func encodeUSNational(t *testing.T, u usNat) string {
	t.Helper()
	w := &bitWriter{}
	w.write(1, 6)  // Version
	w.write(0, 12) // six 2-bit notice fields, all N/A
	w.write(u.sale, 2)
	w.write(u.sharing, 2)
	w.write(u.targeted, 2)
	w.write(0, 8) // trailing padding / later fields we don't read
	return base64.RawURLEncoding.EncodeToString(w.bytes())
}

func TestGPPOptOut(t *testing.T) {
	saleOut := "DBABLA~" + encodeUSNational(t, usNat{sale: 1, sharing: 2, targeted: 2})
	shareOut := "DBABLA~" + encodeUSNational(t, usNat{sale: 2, sharing: 1, targeted: 2})
	targetedOut := "DBABLA~" + encodeUSNational(t, usNat{sale: 2, sharing: 2, targeted: 1})
	allClear := "DBABLA~" + encodeUSNational(t, usNat{sale: 2, sharing: 2, targeted: 2})
	naOnly := "DBABLA~" + encodeUSNational(t, usNat{sale: 0, sharing: 0, targeted: 0})

	tests := []struct {
		name   string
		gpp    string
		gppSID string
		want   bool
	}{
		{"empty", "", "", false},
		{"header only", "DBABLA", "", false},
		{"sale opt-out", saleOut, "7", true},
		{"sharing opt-out", shareOut, "7", true},
		{"targeted opt-out", targetedOut, "7", true},
		{"all did-not-opt-out", allClear, "7", false},
		{"all N/A", naOnly, "7", false},
		{"opt-out but sid says section 8 (unsupported) → no signal", saleOut, "8", false},
		{"gpp_sid space separated", saleOut, "7 9", true},
		{"malformed section base64", "DBABLA~!!!not-base64!!!", "7", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GPPOptOut(tt.gpp, tt.gppSID); got != tt.want {
				t.Errorf("GPPOptOut(%q, %q) = %v, want %v", tt.gpp, tt.gppSID, got, tt.want)
			}
		})
	}
}

func TestParseSectionIDs(t *testing.T) {
	tests := []struct {
		in   string
		want []int
	}{
		{"", nil},
		{"7", []int{7}},
		{"7,8", []int{7, 8}},
		{"8 7", []int{7, 8}}, // sorted ascending to match section order
		{"7, 9, 8", []int{7, 8, 9}},
		{"junk,7", []int{7}},
	}
	for _, tt := range tests {
		got := parseSectionIDs(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("parseSectionIDs(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseSectionIDs(%q) = %v, want %v", tt.in, got, tt.want)
				break
			}
		}
	}
}

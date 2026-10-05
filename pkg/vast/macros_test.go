package vast

import (
	"strings"
	"testing"
	"time"
)

// TestExpandURIMacros_SubstitutesKnownMacros: every bracket macro this
// platform emits is literally replaced with its player-side value, in the
// format VAST 4.2 §6 prescribes ([TIMESTAMP] = ISO 8601 w/ millis, playheads
// = HH:MM:SS.mmm, [CACHEBUSTING] = 8-digit number).
func TestExpandURIMacros_SubstitutesKnownMacros(t *testing.T) {
	now := time.Date(2026, 10, 5, 14, 3, 0, 0, time.UTC)
	uri := "http://t/v1/t/video?tid=x&sig=s&cb=[CACHEBUSTING]&ts=[TIMESTAMP]&pos=[ADPLAYHEAD]&ec=[ERRORCODE]&asset=[ASSETURI]"
	got := ExpandURIMacros(uri, URIMacroValues{
		ErrorCode:  405,
		AdPlayhead: 7*time.Second + 500*time.Millisecond,
		AssetURI:   "https://cdn/x.mp4",
		Now:        now,
		Rand:       func() int64 { return 12345678 },
	})
	for _, want := range []string{
		"cb=12345678",
		"ts=" + "2026-10-05T14%3A03%3A00.000%2B00%3A00",
		"pos=00%3A00%3A07.500",
		"ec=405",
		"asset=https%3A%2F%2Fcdn%2Fx.mp4",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expanded URI missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[") {
		t.Errorf("expanded URI still carries a bracket token: %s", got)
	}
}

// TestExpandURIMacros_LeavesUnknownTokensIntact: per spec a player only
// substitutes macros it supports; unknown tokens pass through literally
// (the server side tolerates them — see the tracker's [ERRORCODE]→900 rule).
func TestExpandURIMacros_LeavesUnknownTokensIntact(t *testing.T) {
	uri := "http://t/x?a=[PODSEQUENCE]&cb=[CACHEBUSTING]"
	got := ExpandURIMacros(uri, URIMacroValues{Rand: func() int64 { return 1 }})
	if !strings.Contains(got, "a=[PODSEQUENCE]") {
		t.Errorf("unknown macro must pass through literally, got %s", got)
	}
	if strings.Contains(got, "[CACHEBUSTING]") {
		t.Errorf("known macro must be substituted, got %s", got)
	}
}

// TestExpandURIMacros_NoBrackets_NoAlloc: URIs without macros return as-is.
func TestExpandURIMacros_NoBrackets_NoAlloc(t *testing.T) {
	uri := "http://t/v1/t/video?tid=x&sig=s"
	if got := ExpandURIMacros(uri, URIMacroValues{}); got != uri {
		t.Errorf("macro-free URI must be unchanged, got %s", got)
	}
}

// TestIsValidErrorCode pins the VAST 4.2 §2.3.6.3 table edges: enumerated
// codes are valid, neighbours and junk are not, 404 is famously NOT a VAST
// code (401/402/403/405 are).
func TestIsValidErrorCode(t *testing.T) {
	cases := map[int]bool{
		99: false, 100: true, 102: true, 103: false,
		206: true, 207: false,
		302: true, 303: true, 305: false,
		400: true, 404: false, 405: true, 410: true, 411: false,
		604: true, 605: false,
		900: true, 901: true, 902: true, 903: false,
		0: false, -1: false, 31337: false,
	}
	for code, want := range cases {
		if got := IsValidErrorCode(code); got != want {
			t.Errorf("IsValidErrorCode(%d) = %v, want %v", code, got, want)
		}
	}
	if ErrorUndefined != 900 {
		t.Errorf("ErrorUndefined must be the spec's 900, got %d", ErrorUndefined)
	}
	if ErrorCodeName(405) == "" || ErrorCodeName(903) != "" {
		t.Errorf("ErrorCodeName: want label for 405, empty for 903")
	}
}

package main

import (
	"net/url"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vast"
)

// TestMediaSigParamsExcludesBracketMacroParams: a signed media beacon with the
// player-substituted bracket-macro params (ec/cb/ts/pos) appended post-signing
// still validates under mediaSigParams, and validating the FULL query fails —
// the exact mirror of the viewability dur/pct/area contract.
func TestMediaSigParamsExcludesBracketMacroParams(t *testing.T) {
	const key = "test-signing-key"
	signed := adserving.SignURL("http://tracker/v1/t/video?tid=t1&cid=c1&advid=a1&event=error", key)

	// The player substituted the macros and fired.
	u, err := url.Parse(signed + "&cb=55443322&ts=2026-10-05T14%3A03%3A00.000%2B00%3A00&pos=00%3A00%3A07.500&ec=405")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if adserving.ValidateSignatureAny(u.Path, q, []string{key}) {
		t.Fatal("full-query validation must fail — macro params are unsigned")
	}
	if !adserving.ValidateSignatureAny(u.Path, mediaSigParams(q), []string{key}) {
		t.Fatal("filtered validation must pass — ec/cb/ts/pos were appended post-signing")
	}

	// A tampered SIGNED param must still invalidate under the filter — the
	// filter list is exactly ec/cb/ts/pos, nothing else escapes the HMAC.
	q.Set("event", "complete")
	if adserving.ValidateSignatureAny(u.Path, mediaSigParams(q), []string{key}) {
		t.Fatal("tampered event= must invalidate even with macro params filtered")
	}

	// No macro params present → q returned unchanged (no-alloc fast path).
	plain, _ := url.Parse(signed)
	pq := plain.Query()
	if !adserving.ValidateSignatureAny(plain.Path, mediaSigParams(pq), []string{key}) {
		t.Fatal("macro-free beacon must validate unchanged")
	}
}

// TestParseErrorCode pins the ec= interpretation rules: valid table code →
// itself; the literal unsubstituted macro → 900 (spec: macro-incapable
// player); junk / out-of-table → 0 (never store attacker-chosen values).
func TestParseErrorCode(t *testing.T) {
	log := nopLog()
	cases := map[string]int{
		"":            0,
		"[ERRORCODE]": vast.ErrorUndefined,
		"405":         405,
		"303":         303,
		"900":         900,
		"31337":       0,
		"404":         0, // not a VAST code
		"abc":         0,
		"-405":        0,
	}
	for raw, want := range cases {
		if got := parseErrorCode(raw, log); got != want {
			t.Errorf("parseErrorCode(%q) = %d, want %d", raw, got, want)
		}
	}
}

// TestParsePlayhead: HH:MM:SS.mmm → ms; the literal macro and garbage → 0.
func TestParsePlayhead(t *testing.T) {
	cases := map[string]int64{
		"00:00:07.500": 7500,
		"01:02:03":     3723000,
		"[ADPLAYHEAD]": 0,
		"garbage":      0,
		"":             0,
	}
	for raw, want := range cases {
		if got := parsePlayhead(raw); got != want {
			t.Errorf("parsePlayhead(%q) = %d, want %d", raw, got, want)
		}
	}
}

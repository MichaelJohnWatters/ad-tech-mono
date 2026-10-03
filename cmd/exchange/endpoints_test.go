package main

import "testing"

func TestSplitDSPEndpoint(t *testing.T) {
	cases := []struct {
		in, endpoint, notify, seat string
	}{
		{"http://dsp-competitor1:8089", "http://dsp-competitor1:8089", "", ""},
		{"grpc://dsp-internal-grpc:8182;notify=http://dsp-internal:8082",
			"grpc://dsp-internal-grpc:8182", "http://dsp-internal:8082", ""},
		{" grpc://a:1 ; notify=http://b:2 ", "grpc://a:1", "http://b:2", ""},
		{"http://a:1;garbage=x", "http://a:1", "", ""},
		// ;seat= alone, and coexisting with ;notify= in either order.
		{"http://p:9;seat=acme-dsp", "http://p:9", "", "acme-dsp"},
		{"http://p:9;seat=acme-dsp;notify=http://p:9",
			"http://p:9", "http://p:9", "acme-dsp"},
		{"grpc://a:1;notify=http://b:2;seat=our-dsp",
			"grpc://a:1", "http://b:2", "our-dsp"},
	}
	for _, c := range cases {
		endpoint, notify, seat := splitDSPEndpoint(c.in)
		if endpoint != c.endpoint || notify != c.notify || seat != c.seat {
			t.Errorf("splitDSPEndpoint(%q) = (%q, %q, %q), want (%q, %q, %q)",
				c.in, endpoint, notify, seat, c.endpoint, c.notify, c.seat)
		}
	}
}

// parseNeverSkip must match router keys (clean bid endpoints) even when the
// operator pastes a full dsp_endpoints entry with its ;notify= suffix — a
// suffixed entry silently never matching would quietly re-expose deal
// demand to the skip rules.
func TestParseNeverSkipNormalisesEntries(t *testing.T) {
	set := parseNeverSkip("grpc://dsp-internal-grpc:8182;notify=http://dsp-internal:8082, http://partner:9100 ,")
	if _, ok := set["grpc://dsp-internal-grpc:8182"]; !ok {
		t.Error("suffixed entry must normalise to the clean bid endpoint")
	}
	if _, ok := set["http://partner:9100"]; !ok {
		t.Error("plain entry must be kept verbatim (trimmed)")
	}
	if len(set) != 2 {
		t.Errorf("set size = %d, want 2 (empty trailing entry dropped)", len(set))
	}
}

// addInternalNeverSkip must exempt OUR OWN (grpc://) demand from the skip
// rules automatically — a learned skip of the internal DSP starves every
// platform advertiser — while leaving external (http://) entries subject to
// routing and preserving anything the operator configured explicitly.
func TestAddInternalNeverSkipExemptsOwnDemand(t *testing.T) {
	endpoints := "grpc://dsp-internal-grpc:8182;notify=http://dsp-internal:8082,http://dsp-competitor1:8089,http://dsp-competitor2:8090"

	// nil configured set → internal endpoint still exempted, externals not.
	set := addInternalNeverSkip(nil, endpoints)
	if _, ok := set["grpc://dsp-internal-grpc:8182"]; !ok {
		t.Error("internal grpc:// endpoint must be auto-exempted from skip rules")
	}
	if _, ok := set["http://dsp-competitor1:8089"]; ok {
		t.Error("external http:// endpoints must stay subject to routing")
	}
	if len(set) != 1 {
		t.Errorf("set size = %d, want 1 (internal only)", len(set))
	}

	// Configured entries (external deal-holders) survive alongside.
	set = addInternalNeverSkip(parseNeverSkip("http://partner:9100"), endpoints)
	for _, want := range []string{"grpc://dsp-internal-grpc:8182", "http://partner:9100"} {
		if _, ok := set[want]; !ok {
			t.Errorf("merged set missing %q", want)
		}
	}

	// No grpc:// entries (external-only deployment) → nil stays nil.
	if set := addInternalNeverSkip(nil, "http://a:1,http://b:2"); set != nil {
		t.Errorf("no internal endpoints should leave the set untouched, got %v", set)
	}
}

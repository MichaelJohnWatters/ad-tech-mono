package main

import "testing"

func TestSplitDSPEndpoint(t *testing.T) {
	cases := []struct {
		in, endpoint, notify string
	}{
		{"http://dsp-competitor1:8089", "http://dsp-competitor1:8089", ""},
		{"grpc://dsp-internal-grpc:8182;notify=http://dsp-internal:8082",
			"grpc://dsp-internal-grpc:8182", "http://dsp-internal:8082"},
		{" grpc://a:1 ; notify=http://b:2 ", "grpc://a:1", "http://b:2"},
		{"http://a:1;garbage=x", "http://a:1", ""},
	}
	for _, c := range cases {
		endpoint, notify := splitDSPEndpoint(c.in)
		if endpoint != c.endpoint || notify != c.notify {
			t.Errorf("splitDSPEndpoint(%q) = (%q, %q), want (%q, %q)",
				c.in, endpoint, notify, c.endpoint, c.notify)
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

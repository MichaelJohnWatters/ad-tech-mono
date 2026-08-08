package clientip

import (
	"net/http/httptest"
	"testing"
)

func TestResolve(t *testing.T) {
	for _, tc := range []struct {
		name       string
		xff        string
		xRealIP    string
		remoteAddr string
		hops       int
		want       string
	}{
		{name: "remoteaddr only strips port", remoteAddr: "10.0.0.9:4431", want: "10.0.0.9"},
		{name: "x-real-ip beats remoteaddr", xRealIP: "203.0.113.7", remoteAddr: "10.0.0.9:4431", want: "203.0.113.7"},
		{name: "single xff entry", xff: "198.51.100.4", remoteAddr: "10.0.0.9:4431", want: "198.51.100.4"},
		{name: "hops=0 takes rightmost (client-prepended junk ignored)", xff: "6.6.6.6, 198.51.100.4", want: "198.51.100.4"},
		{name: "hops=1 skips the ingress-appended entry", xff: "203.0.113.7, 10.42.0.5", hops: 1, want: "203.0.113.7"},
		{name: "hops beyond entries clamps to leftmost", xff: "198.51.100.4", hops: 3, want: "198.51.100.4"},
		{name: "whitespace trimmed", xff: " 6.6.6.6 ,  198.51.100.4 ", want: "198.51.100.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xRealIP != "" {
				r.Header.Set("X-Real-IP", tc.xRealIP)
			}
			if tc.remoteAddr != "" {
				r.RemoteAddr = tc.remoteAddr
			}
			if got := Resolve(r, tc.hops); got != tc.want {
				t.Errorf("Resolve() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAllowlistMatch(t *testing.T) {
	var a Allowlist
	cidrs := "127.0.0.1, 10.0.0.0/8, fc00::/7, garbage, 192.168.1.5"
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},   // bare IPv4 → /32
		{"127.0.0.2", false},  // outside the /32
		{"10.42.0.17", true},  // CIDR match
		{"fc00::1", true},     // IPv6 CIDR
		{"192.168.1.5", true}, // bare IP later in list (after a malformed token)
		{"192.168.1.6", false},
		{"not-an-ip", false},
	} {
		if got := a.Match(tc.ip, cidrs); got != tc.want {
			t.Errorf("Match(%q) = %v, want %v", tc.ip, got, tc.want)
		}
	}
	// Cache invalidation: a changed list takes effect.
	if a.Match("10.42.0.17", "192.0.2.0/24") {
		t.Error("Match should not hit after the allowlist string changed")
	}
	if !a.Match("192.0.2.9", "192.0.2.0/24") {
		t.Error("Match should hit the new allowlist")
	}
	if a.Match("10.42.0.17", "") {
		t.Error("empty allowlist must never match")
	}
}

package main

import (
	"net/http"
	"testing"
)

func TestClientIP(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		xRealIP    string
		want       string
	}{
		{"xff single", "10.0.0.1:5000", "203.0.113.7", "", "203.0.113.7"},
		{"xff chain uses first (client)", "10.0.0.1:5000", "203.0.113.7, 70.41.3.18, 10.0.0.1", "", "203.0.113.7"},
		{"xff trims whitespace", "10.0.0.1:5000", "  203.0.113.9 ,10.0.0.1", "", "203.0.113.9"},
		{"x-real-ip fallback", "10.0.0.1:5000", "", "198.51.100.5", "198.51.100.5"},
		{"remoteaddr strips port", "192.0.2.44:54321", "", "", "192.0.2.44"},
		{"xff wins over x-real-ip", "10.0.0.1:5000", "203.0.113.7", "198.51.100.5", "203.0.113.7"},
		{"remoteaddr without port passes through", "192.0.2.44", "", "", "192.0.2.44"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{RemoteAddr: tc.remoteAddr, Header: http.Header{}}
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xRealIP != "" {
				r.Header.Set("X-Real-IP", tc.xRealIP)
			}
			if got := clientIP(r); got != tc.want {
				t.Errorf("clientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

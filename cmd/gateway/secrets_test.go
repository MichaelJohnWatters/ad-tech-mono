package main

import "testing"

// These tests cover the small pure helpers in secrets.go that don't
// need a Postgres / NATS round-trip. The full CRUD path is exercised
// by e2e in tests/e2e/secrets_crud_test.go (added alongside this
// handler).
func TestMaskValue(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"abc", "***"},
		{"abcdef", "******"},
		{"abcdefg", "abcdef…"},
		{"4f3a2b1c0e9d8c7b6a5f4e3d2c1b0a", "4f3a2b…"},
	}
	for _, tc := range cases {
		if got := maskValue(tc.in); got != tc.want {
			t.Errorf("maskValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsValidStatus(t *testing.T) {
	if !isValidStatus("active") || !isValidStatus("rotating") || !isValidStatus("revoked") {
		t.Error("known statuses should validate")
	}
	if isValidStatus("paused") || isValidStatus("") {
		t.Error("unknown statuses should reject")
	}
}

func TestIsValidPurpose(t *testing.T) {
	for _, p := range []string{"api_key", "jwt_signing", "hmac_tracker", "partner_shared", "service_s2s"} {
		if !isValidPurpose(p) {
			t.Errorf("purpose %q should validate", p)
		}
	}
	if isValidPurpose("admin_key") || isValidPurpose("") {
		t.Error("unknown purposes should reject")
	}
}

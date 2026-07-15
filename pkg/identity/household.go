package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// HouseholdIDPrefix marks a platform household identifier. Audience segment
// members, identity-graph nodes and bid-request EIDs all carry the prefixed
// form, so a household id is never confused with a user id.
const HouseholdIDPrefix = "hh:"

// HouseholdID derives the platform household identifier from a client IP —
// the industry's de facto household proxy for CTV. HMAC-salted so the id
// can't be reversed to an IP (the IPv4 space is small enough to enumerate a
// bare hash); truncated to 16 hex chars, which is plenty for household-scale
// cardinality. Deterministic and time-free: the same IP yields the same
// household across days, and every deriver (SSP, seed, tests) must use THIS
// function with the same salt so ids line up.
//
// Returns "" for an empty IP — callers treat that as "no household signal".
func HouseholdID(salt, ip string) string {
	if ip == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write([]byte(ip))
	return HouseholdIDPrefix + hex.EncodeToString(mac.Sum(nil))[:16]
}

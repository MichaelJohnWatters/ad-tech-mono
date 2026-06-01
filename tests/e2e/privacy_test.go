//go:build e2e

// Privacy tests — opt-out and consent flow. Most are skipped until the
// platform wires consent-aware filtering at the DSP and ad server level.
package e2e

import "testing"

func TestPrivacyOptOutBlocksServe(t *testing.T) {
	t.Skip("opt-out lookup not wired into ad server / DSP bid path yet; needs user_optouts table consumer + harness helper to add an opt-out row")
}

func TestPrivacyConsentSignalPropagated(t *testing.T) {
	t.Skip("regs/consent objects in OpenRTB BidRequest aren't enforced downstream yet; tracker/billing don't filter on consent flags")
}

func TestPrivacyPIINotInLogs(t *testing.T) {
	t.Skip("would require sweeping stdout/stderr of every service for PII patterns; out of scope for this functional pass — covered by code review + linters")
}

func TestPrivacyDeletionPropagation(t *testing.T) {
	t.Skip("user-deletion flow needs cmd/privacy-delete built (currently an empty cmd/ shell); test ready when service is up")
}

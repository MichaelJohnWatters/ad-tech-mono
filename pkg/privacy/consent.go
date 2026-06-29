package privacy

// This file holds the stateless, hot-path consent decision used by the DSP
// bid handler (and any other serving path). It is deliberately separate
// from Manager: serving needs a fast, allocation-free verdict from a warm
// cache + the inbound OpenRTB signals, not the full opt-out/deletion
// machinery.

// OptOut is the minimal warm-cache row: a user and their registry opt-out
// level. Loaded from opt_out_registry and indexed by UserID.
type OptOut struct {
	UserID string
	Level  OptOutLevel
}

// LevelFromInt maps a stored integer (opt_out_registry.level is 1..3) to an
// OptOutLevel, clamping anything out of range to LevelNone so a bad row
// can't accidentally block (or over-permit) serving.
func LevelFromInt(n int) OptOutLevel {
	switch n {
	case 1:
		return LevelNoPersonalisation
	case 2:
		return LevelNoTracking
	case 3:
		return LevelFullDeletion
	default:
		return LevelNone
	}
}

// Decision is the consent verdict for a single bid request.
type Decision struct {
	// Bid is false when the DSP must not bid at all (the user opted out of
	// tracking/targeting entirely).
	Bid bool
	// Personalise is false when the DSP may bid but only contextually —
	// behavioural/audience targeting must be stripped.
	Personalise bool
	// Reason is a short machine-readable tag for logs/metrics.
	Reason string
}

// Evaluate combines the platform opt-out registry level with the inbound
// OpenRTB regulatory signals into a single verdict. Precedence:
//
//  1. Registry opt-out is the strongest signal (the user told *us*).
//     Level 2/3 → no bid; Level 1 → contextual only.
//  2. COPPA (child-directed) → contextual only.
//  3. GDPR applies but no TCF consent string present → contextual only.
//  4. US Privacy "opt-out of sale" set → contextual only.
//  5. Otherwise → full personalisation.
//
// We deliberately downgrade-to-contextual rather than no-bid for the
// regulatory signals (2–4): serving a non-personalised ad is lawful and
// keeps inventory monetised, whereas a blanket no-bid would silently drop
// all EU / child / CCPA-opt-out traffic.
func Evaluate(level OptOutLevel, gdpr int, tcfConsent, usPrivacy string, coppa int) Decision {
	switch {
	case level >= LevelNoTracking:
		return Decision{Bid: false, Personalise: false, Reason: "opt_out_no_tracking"}
	case level == LevelNoPersonalisation:
		return Decision{Bid: true, Personalise: false, Reason: "opt_out_no_personalisation"}
	}
	if coppa == 1 {
		return Decision{Bid: true, Personalise: false, Reason: "coppa"}
	}
	if gdpr == 1 && tcfConsent == "" {
		return Decision{Bid: true, Personalise: false, Reason: "gdpr_no_consent"}
	}
	if usPrivacyOptOut(usPrivacy) {
		return Decision{Bid: true, Personalise: false, Reason: "us_privacy_opt_out"}
	}
	return Decision{Bid: true, Personalise: true, Reason: "consented"}
}

// usPrivacyOptOut parses the IAB US Privacy ("CCPA") string, e.g. "1YNN":
//
//	char 1 — spec version
//	char 2 — explicit notice given
//	char 3 — opt-out of sale  ← 'Y' means the user opted out
//	char 4 — LSPA covered
//
// Anything that isn't a well-formed 4-char string is treated as "no
// opt-out signal" (we don't infer an opt-out from a malformed value).
func usPrivacyOptOut(s string) bool {
	return len(s) == 4 && (s[2] == 'Y' || s[2] == 'y')
}

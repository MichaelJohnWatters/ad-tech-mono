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

// Signals is the full set of privacy inputs for a single bid request: the
// platform's own opt-out registry level plus every inbound regulatory signal
// carried on the OpenRTB request. It's a struct rather than a positional
// parameter list because the signal set keeps growing (GPC, GPP, …) and named
// fields keep call sites unambiguous.
type Signals struct {
	// Level is the platform opt-out registry level for the user (LevelNone
	// when unknown / no row).
	Level OptOutLevel
	// GDPR is regs.ext.gdpr (1 = GDPR applies); TCFConsent is the TCF string.
	GDPR       int
	TCFConsent string
	// USPrivacy is the legacy IAB US Privacy ("CCPA") string, e.g. "1YNN".
	USPrivacy string
	// COPPA is regs.coppa (1 = child-directed).
	COPPA int
	// GPC is the Global Privacy Control browser signal (regs.ext.gpc == 1) —
	// a first-class "do not sell/share" request.
	GPC bool
	// GPP / GPPSID are the IAB Global Privacy Platform consent string and its
	// section-id list (regs.ext.gpp / gpp_sid).
	GPP    string
	GPPSID string
}

// Evaluate combines the platform opt-out registry level with the inbound
// OpenRTB regulatory signals into a single verdict. Precedence:
//
//  1. Registry opt-out is the strongest signal (the user told *us*).
//     Level 2/3 → no bid; Level 1 → contextual only.
//  2. COPPA (child-directed) → contextual only.
//  3. GDPR applies but no TCF consent string present → contextual only.
//  4. GPC "do not sell/share" browser signal → contextual only.
//  5. US Privacy "opt-out of sale" set → contextual only.
//  6. GPP US section signals a sale/share/targeted-ad opt-out → contextual only.
//  7. Otherwise → full personalisation.
//
// We deliberately downgrade-to-contextual rather than no-bid for the
// regulatory signals (2–6): serving a non-personalised ad is lawful and
// keeps inventory monetised, whereas a blanket no-bid would silently drop
// all EU / child / CCPA-opt-out traffic.
func Evaluate(s Signals) Decision {
	switch {
	case s.Level >= LevelNoTracking:
		return Decision{Bid: false, Personalise: false, Reason: "opt_out_no_tracking"}
	case s.Level == LevelNoPersonalisation:
		return Decision{Bid: true, Personalise: false, Reason: "opt_out_no_personalisation"}
	}
	if s.COPPA == 1 {
		return Decision{Bid: true, Personalise: false, Reason: "coppa"}
	}
	if s.GDPR == 1 && s.TCFConsent == "" {
		return Decision{Bid: true, Personalise: false, Reason: "gdpr_no_consent"}
	}
	if s.GPC {
		return Decision{Bid: true, Personalise: false, Reason: "gpc"}
	}
	if usPrivacyOptOut(s.USPrivacy) {
		return Decision{Bid: true, Personalise: false, Reason: "us_privacy_opt_out"}
	}
	if GPPOptOut(s.GPP, s.GPPSID) {
		return Decision{Bid: true, Personalise: false, Reason: "gpp_opt_out"}
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

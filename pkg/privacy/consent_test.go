package privacy

import "testing"

func TestEvaluate(t *testing.T) {
	// usNatOptOut is a GPP string whose US National section signals a sale
	// opt-out (built via the test encoder in gpp_test.go).
	usNatOptOut := "DBABLA~" + encodeUSNational(t, usNat{sale: 1})
	usNatNoOptOut := "DBABLA~" + encodeUSNational(t, usNat{sale: 2})

	cases := []struct {
		name            string
		sig             Signals
		wantBid         bool
		wantPersonalise bool
		wantReason      string
	}{
		{"no opt-out, no regs → full", Signals{}, true, true, "consented"},
		{"level 1 → contextual only", Signals{Level: LevelNoPersonalisation}, true, false, "opt_out_no_personalisation"},
		{"level 2 → no bid", Signals{Level: LevelNoTracking}, false, false, "opt_out_no_tracking"},
		{"level 3 → no bid", Signals{Level: LevelFullDeletion}, false, false, "opt_out_no_tracking"},
		{"coppa → contextual", Signals{COPPA: 1}, true, false, "coppa"},
		{"gdpr + no consent → contextual", Signals{GDPR: 1}, true, false, "gdpr_no_consent"},
		{"gdpr + consent → full", Signals{GDPR: 1, TCFConsent: "CONSENT_STR"}, true, true, "consented"},
		{"gpc → contextual", Signals{GPC: true}, true, false, "gpc"},
		{"us privacy opt-out → contextual", Signals{USPrivacy: "1YYN"}, true, false, "us_privacy_opt_out"},
		{"us privacy no opt-out → full", Signals{USPrivacy: "1YNN"}, true, true, "consented"},
		{"malformed us privacy ignored", Signals{USPrivacy: "1"}, true, true, "consented"},
		{"gpp us-national opt-out → contextual", Signals{GPP: usNatOptOut, GPPSID: "7"}, true, false, "gpp_opt_out"},
		{"gpp us-national no opt-out → full", Signals{GPP: usNatNoOptOut, GPPSID: "7"}, true, true, "consented"},
		// Registry opt-out beats a permissive regs signal.
		{"level 2 beats consent string", Signals{Level: LevelNoTracking, GDPR: 1, TCFConsent: "CONSENT", USPrivacy: "1YNN"}, false, false, "opt_out_no_tracking"},
		// GPC takes precedence over an otherwise-consented US privacy string.
		{"gpc beats permissive us privacy", Signals{GPC: true, USPrivacy: "1YNN"}, true, false, "gpc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Evaluate(tc.sig)
			if d.Bid != tc.wantBid || d.Personalise != tc.wantPersonalise {
				t.Errorf("Evaluate = {Bid:%v Personalise:%v}, want {Bid:%v Personalise:%v}",
					d.Bid, d.Personalise, tc.wantBid, tc.wantPersonalise)
			}
			if d.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", d.Reason, tc.wantReason)
			}
		})
	}
}

func TestLevelFromInt(t *testing.T) {
	cases := map[int]OptOutLevel{
		0: LevelNone, 1: LevelNoPersonalisation, 2: LevelNoTracking,
		3: LevelFullDeletion, 99: LevelNone, -1: LevelNone,
	}
	for in, want := range cases {
		if got := LevelFromInt(in); got != want {
			t.Errorf("LevelFromInt(%d) = %d, want %d", in, got, want)
		}
	}
}

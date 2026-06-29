package privacy

import "testing"

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name            string
		level           OptOutLevel
		gdpr            int
		consent         string
		usPrivacy       string
		coppa           int
		wantBid         bool
		wantPersonalise bool
		wantReason      string
	}{
		{"no opt-out, no regs → full", LevelNone, 0, "", "", 0, true, true, "consented"},
		{"level 1 → contextual only", LevelNoPersonalisation, 0, "", "", 0, true, false, "opt_out_no_personalisation"},
		{"level 2 → no bid", LevelNoTracking, 0, "", "", 0, false, false, "opt_out_no_tracking"},
		{"level 3 → no bid", LevelFullDeletion, 0, "", "", 0, false, false, "opt_out_no_tracking"},
		{"coppa → contextual", LevelNone, 0, "", "", 1, true, false, "coppa"},
		{"gdpr + no consent → contextual", LevelNone, 1, "", "", 0, true, false, "gdpr_no_consent"},
		{"gdpr + consent → full", LevelNone, 1, "CONSENT_STR", "", 0, true, true, "consented"},
		{"us privacy opt-out → contextual", LevelNone, 0, "", "1YYN", 0, true, false, "us_privacy_opt_out"},
		{"us privacy no opt-out → full", LevelNone, 0, "", "1YNN", 0, true, true, "consented"},
		{"malformed us privacy ignored", LevelNone, 0, "", "1", 0, true, true, "consented"},
		// Registry opt-out beats a permissive regs signal.
		{"level 2 beats consent string", LevelNoTracking, 1, "CONSENT", "1YNN", 0, false, false, "opt_out_no_tracking"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Evaluate(tc.level, tc.gdpr, tc.consent, tc.usPrivacy, tc.coppa)
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

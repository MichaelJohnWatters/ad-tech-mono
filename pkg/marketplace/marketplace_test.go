package marketplace

import "testing"

func TestIsValidStatus(t *testing.T) {
	for _, s := range []string{StatusActive, StatusPaused, StatusWithdrawn} {
		if !IsValidStatus(s) {
			t.Errorf("IsValidStatus(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "sold", "deleted", "ACTIVE"} {
		if IsValidStatus(s) {
			t.Errorf("IsValidStatus(%q) = true, want false", s)
		}
	}
}

func TestCPMSurchargeUSD(t *testing.T) {
	cases := []struct {
		micros int64
		want   float64
	}{{500_000, 0.50}, {750_000, 0.75}, {1_000_000, 1.0}, {0, 0}}
	for _, c := range cases {
		if got := (Listing{CPMSurchargeMicros: c.micros}).CPMSurchargeUSD(); got != c.want {
			t.Errorf("CPMSurchargeUSD(%d) = %v, want %v", c.micros, got, c.want)
		}
	}
}

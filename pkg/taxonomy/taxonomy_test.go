package taxonomy

import (
	"reflect"
	"testing"
)

func TestCompleteTier1(t *testing.T) {
	// The taxonomy must be the full IAB Content Taxonomy 1.0 tier-1 set:
	// exactly IAB1..IAB26, contiguous, no gaps.
	all := All()
	if len(all) != 26 {
		t.Fatalf("tier-1 count = %d, want 26", len(all))
	}
	for i, c := range all {
		want := "IAB" + itoa(i+1)
		if c.Code != want {
			t.Errorf("All()[%d].Code = %q, want %q (must be contiguous IAB1..IAB26)", i, c.Code, want)
		}
		if c.Name == "" {
			t.Errorf("%s has no name", c.Code)
		}
	}
}

func TestSpecAccurateCodes(t *testing.T) {
	// Guard against the historical bug where IAB10 (Home & Garden) was used
	// for property; Real Estate is IAB21.
	cases := map[string]string{
		"IAB10": "Home & Garden",
		"IAB21": "Real Estate",
		"IAB12": "News",
		"IAB13": "Personal Finance",
		"IAB1":  "Arts & Entertainment",
	}
	for code, want := range cases {
		got, ok := Name(code)
		if !ok || got != want {
			t.Errorf("Name(%q) = %q,%v; want %q,true", code, got, ok, want)
		}
	}
}

func TestTier1(t *testing.T) {
	cases := map[string]string{
		"IAB17":    "IAB17",
		"IAB17-1":  "IAB17",
		"IAB17-44": "IAB17",
		" IAB2 ":   "IAB2",
		"garbage":  "garbage",
	}
	for in, want := range cases {
		if got := Tier1(in); got != want {
			t.Errorf("Tier1(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsValid(t *testing.T) {
	valid := []string{"IAB1", "IAB26", "IAB17-1", "IAB17-999", " IAB5 "}
	invalid := []string{"", "IAB0", "IAB27", "IAB99", "IABxx", "12", "IAB"}
	for _, c := range valid {
		if !IsValid(c) {
			t.Errorf("IsValid(%q) = false, want true", c)
		}
	}
	for _, c := range invalid {
		if IsValid(c) {
			t.Errorf("IsValid(%q) = true, want false", c)
		}
	}
}

func TestWithParents(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, nil},
		{"tier1 only unchanged", []string{"IAB1"}, []string{"IAB1"}},
		{"tier2 gains parent", []string{"IAB17-1"}, []string{"IAB17-1", "IAB17"}},
		{"parent already present, no dup", []string{"IAB17", "IAB17-1"}, []string{"IAB17", "IAB17-1"}},
		{"dedupe", []string{"IAB2", "IAB2"}, []string{"IAB2"}},
		{"unknown passed through", []string{"IAB99-3"}, []string{"IAB99-3"}},
		{"mixed", []string{"IAB17-1", "IAB20"}, []string{"IAB17-1", "IAB17", "IAB20"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithParents(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("WithParents(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// itoa is a tiny local int→string to avoid importing strconv just for the test.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

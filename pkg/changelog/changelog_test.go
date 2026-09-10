package changelog

import "testing"

func TestValidCategory(t *testing.T) {
	for _, c := range []string{"added", "changed", "deprecated", "removed", "fixed", "security"} {
		if !ValidCategory(c) {
			t.Errorf("%q should be valid", c)
		}
	}
	for _, c := range []string{"", "Added", "breaking", "misc", "SECURITY"} {
		if ValidCategory(c) {
			t.Errorf("%q should be invalid", c)
		}
	}
}

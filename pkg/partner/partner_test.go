package partner

import "testing"

func TestCanTransition(t *testing.T) {
	ok := [][2]string{
		{StatusPending, StatusSandbox},
		{StatusSandbox, StatusCertified},
		{StatusCertified, StatusActive},
		{StatusActive, StatusPaused},
		{StatusPaused, StatusActive},
		{StatusSandbox, StatusTerminated},
		{StatusCertified, StatusSandbox}, // send back for re-test
	}
	for _, c := range ok {
		if !CanTransition(c[0], c[1]) {
			t.Errorf("CanTransition(%s,%s) = false, want true", c[0], c[1])
		}
	}
	bad := [][2]string{
		{StatusPending, StatusActive},    // can't skip certification
		{StatusPending, StatusCertified}, // can't skip sandbox
		{StatusSandbox, StatusActive},    // must certify first
		{StatusTerminated, StatusActive}, // terminal
		{StatusActive, StatusSandbox},    // no direct downgrade to sandbox
		{"bogus", StatusActive},
		{StatusActive, "bogus"},
	}
	for _, c := range bad {
		if CanTransition(c[0], c[1]) {
			t.Errorf("CanTransition(%s,%s) = true, want false", c[0], c[1])
		}
	}
}

func TestValidators(t *testing.T) {
	if !IsValidKind(KindDSP) || !IsValidKind(KindSSP) || IsValidKind("cdp") {
		t.Error("IsValidKind wrong")
	}
	for _, s := range []string{StatusPending, StatusSandbox, StatusCertified, StatusActive, StatusPaused, StatusTerminated} {
		if !IsValidStatus(s) {
			t.Errorf("IsValidStatus(%s) = false", s)
		}
	}
	if IsValidStatus("live") {
		t.Error("IsValidStatus(live) = true, want false")
	}
}

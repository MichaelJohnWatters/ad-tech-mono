package middleware

import (
	"net/http"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

func TestParseActAsTarget(t *testing.T) {
	cases := []struct {
		in       string
		wantType auth.AccountType
		wantID   string
	}{
		{"advertiser:acc-1", auth.AccountAdvertiser, "acc-1"},
		{"publisher:acc-2", auth.AccountPublisher, "acc-2"},
		{"acc-3", auth.AccountAdvertiser, "acc-3"},       // agency legacy (bare id)
		{"staff:acc-4", auth.AccountAdvertiser, "acc-4"}, // unrecognised type prefix → advertiser
		{"advertiser:", auth.AccountAdvertiser, ""},      // empty id
	}
	for _, c := range cases {
		gt, gid := ParseActAsTarget(c.in)
		if gt != c.wantType || gid != c.wantID {
			t.Errorf("ParseActAsTarget(%q) = (%s, %q), want (%s, %q)", c.in, gt, gid, c.wantType, c.wantID)
		}
	}
}

func TestActAsTarget_HeaderWinsThenCookie(t *testing.T) {
	// Header takes precedence.
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set(constants.HeaderActAs, "advertiser:from-header")
	r.AddCookie(&http.Cookie{Name: "act_as_account", Value: "advertiser:from-cookie"})
	if got := ActAsTarget(r); got != "advertiser:from-header" {
		t.Errorf("header should win: got %q", got)
	}

	// Cookie fallback when no header.
	r2, _ := http.NewRequest("GET", "/", nil)
	r2.AddCookie(&http.Cookie{Name: "act_as_account", Value: "publisher:from-cookie"})
	if got := ActAsTarget(r2); got != "publisher:from-cookie" {
		t.Errorf("cookie fallback: got %q", got)
	}

	// Neither → empty.
	r3, _ := http.NewRequest("GET", "/", nil)
	if got := ActAsTarget(r3); got != "" {
		t.Errorf("no act-as: got %q", got)
	}
}

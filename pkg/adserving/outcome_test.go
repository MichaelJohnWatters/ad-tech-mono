package adserving

import (
	"net/http/httptest"
	"testing"
)

func TestOutcomeStringOmitsEmptyAndZero(t *testing.T) {
	got := Outcome{Result: OutcomeFill, Type: "video", Advertiser: "ford.com", Price: 7.8, Currency: "USD", Model: "cpm"}.String()
	want := "result=fill; type=video; adv=ford.com; price=7.80; cur=USD; model=cpm"
	if got != want {
		t.Fatalf("String()\n got=%q\nwant=%q", got, want)
	}
	// Zero price + empty fields drop out entirely.
	if g := (Outcome{Result: OutcomeNoBid, Type: "display", Reason: "no-demand"}).String(); g != "result=nobid; type=display; reason=no-demand" {
		t.Fatalf("nobid String()=%q", g)
	}
}

func TestOutcomeRendersSegments(t *testing.T) {
	got := Outcome{Result: OutcomeFill, Type: "display", Segments: []string{"sports_fans", "auto_intenders"}}.String()
	want := "result=fill; type=display; seg=sports_fans,auto_intenders"
	if got != want {
		t.Fatalf("segments String()\n got=%q\nwant=%q", got, want)
	}
	// No segments → no seg key at all.
	if g := (Outcome{Result: OutcomeFill, Type: "display"}).String(); g != "result=fill; type=display" {
		t.Fatalf("empty-segments String()=%q", g)
	}
}

func TestOutcomeSanitizesSeparators(t *testing.T) {
	// A deal id with the grammar separators must not corrupt the header.
	got := Outcome{Result: OutcomeFill, Type: "video", Deal: "a;b=c\nd"}.String()
	want := "result=fill; type=video; deal=a,b-c d"
	if got != want {
		t.Fatalf("sanitize String()=%q want %q", got, want)
	}
}

func TestSetOutcomeNoopOnEmpty(t *testing.T) {
	w := httptest.NewRecorder()
	SetOutcome(w, Outcome{})
	if v := w.Header().Get(OutcomeHeader); v != "" {
		t.Fatalf("expected no header on empty Outcome, got %q", v)
	}
	SetOutcome(w, Outcome{Result: OutcomeContent, Type: "ctv"})
	if v := w.Header().Get(OutcomeHeader); v != "result=content; type=ctv" {
		t.Fatalf("header=%q", v)
	}
}

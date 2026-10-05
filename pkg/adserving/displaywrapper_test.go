package adserving

import (
	"strings"
	"testing"
)

// TestWrapExternalDisplayHTML pins the wrapper contract: buyer HTML verbatim
// inside a labelled div, the 1×1 impression <img>, and the viewability
// observer script with the URL as a safe JS string literal.
func TestWrapExternalDisplayHTML(t *testing.T) {
	buyer := `<div data-buyer="1" onclick="go()">EXTERNAL DSP won @ 4.2500</div>`
	imp := "http://tracker/v1/t/imp?tid=t1&sig=s"
	view := `http://tracker/v1/t/view?tid=t1&sig=s"</script>` // hostile chars must not escape the JS literal

	out := WrapExternalDisplayHTML(buyer, imp, view, "data-external-adm-wrapper")
	for _, want := range []string{
		`<div data-external-adm-wrapper="1"`,
		buyer,
		`<img src="` + imp + `"`,
		"IntersectionObserver",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("wrapper missing %q:\n%s", want, out)
		}
	}
	// The viewability URL rides through json.Marshal — the raw `"</script>`
	// must not appear unescaped (script-breakout guard).
	if strings.Contains(out, `=s"</script>`) {
		t.Errorf("viewability URL not JS-escaped:\n%s", out)
	}

	// The Prebid call site keeps its historical attribute.
	if !strings.Contains(WrapExternalDisplayHTML(buyer, imp, view, "data-prebid-wrapper"), `data-prebid-wrapper="1"`) {
		t.Error("wrapper attribute must be caller-selectable")
	}
}

// TestViewabilityBeaconScript: the script carries the signed URL, appends
// the client-measured dur/pct/area as unsigned params, and fires via Image().
func TestViewabilityBeaconScript(t *testing.T) {
	s := ViewabilityBeaconScript("http://t/v1/t/view?tid=x&sig=y")
	for _, want := range []string{"<script>", "http://t/v1/t/view?tid=x", "'dur='+d", "'&pct='+p", "'&area='+a", "new Image()"} {
		if !strings.Contains(s, want) {
			t.Errorf("beacon script missing %q:\n%s", want, s)
		}
	}
}

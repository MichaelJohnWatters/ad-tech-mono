package main

import (
	"os"
	"regexp"
	"testing"
)

// TestPortalCatalogMatchesEventRoutes pins the portals' webhook event catalog
// (web/static/portal-webhooks.js WEBHOOK_EVENTS — the ONE copy both portals
// render) against this dispatcher's eventRoutes. The catalog used to exist in
// three places (here + two templates) and templates have no unit tests, so an
// event added to one silently missed the others. Now: add an event to
// eventRoutes without the portal catalog (or vice versa) and this fails.
func TestPortalCatalogMatchesEventRoutes(t *testing.T) {
	src, err := os.ReadFile("../../web/static/portal-webhooks.js")
	if err != nil {
		t.Fatalf("read portal-webhooks.js: %v", err)
	}
	re := regexp.MustCompile(`\{ev:\s*'([a-z_.]+)'`)
	js := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		js[m[1]] = true
	}
	if len(js) == 0 {
		t.Fatal("no WEBHOOK_EVENTS entries parsed from portal-webhooks.js — extractor broke")
	}
	for _, ev := range eventRoutes {
		if !js[ev] {
			t.Errorf("event %q is routed by the dispatcher but missing from the portal catalog (portal-webhooks.js WEBHOOK_EVENTS)", ev)
		}
	}
	routed := map[string]bool{}
	for _, ev := range eventRoutes {
		routed[ev] = true
	}
	for ev := range js {
		if !routed[ev] {
			t.Errorf("event %q is offered in the portal catalog but no NATS subject routes to it (cmd/webhooks eventRoutes)", ev)
		}
	}
}

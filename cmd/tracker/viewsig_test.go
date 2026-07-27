package main

import (
	"net/url"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
)

// TestViewSigParamsExcludesMeasurement proves a properly-signed viewability URL
// still validates after the browser appends the client-measured dur/pct/area —
// the params were never part of the signed message (BuildViewabilityURL signs
// tid/cid/pid/pubid/uid/exp only), so validating the FULL query would 403 every
// real beacon under strict signing.
func TestViewSigParamsExcludesMeasurement(t *testing.T) {
	const key = "test-signing-key"
	base := "http://tracker/v1/t/view?tid=t1&cid=c1&pid=p1&pubid=pub1&exp=9999999999"
	signed := adserving.SignURL(base, key) // appends &sig=<hmac over the above>

	// The browser (adtech.js / the injected Prebid beacon) appends the measured
	// dur/pct/area AFTER the URL was signed.
	u, err := url.Parse(signed + "&dur=1200&pct=75&area=90000")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()

	// Validating the FULL query (the old behaviour) must FAIL — the appended
	// params corrupt the recomputed signature.
	if adserving.ValidateSignatureAny(u.Path, q, []string{key}) {
		t.Fatal("raw full-query validation unexpectedly PASSED with appended measurement params")
	}

	// Excluding the client-measured params must PASS — this is the fix.
	if !adserving.ValidateSignatureAny(u.Path, viewSigParams(q), []string{key}) {
		t.Fatal("viewSigParams validation FAILED; a signed viewability beacon would 403 under strict signing")
	}
}

// TestViewSigParamsNoMeasurementUnchanged: with no measurement params, the query
// is returned as-is (a signed URL with no client additions still validates).
func TestViewSigParamsNoMeasurementUnchanged(t *testing.T) {
	const key = "test-signing-key"
	signed := adserving.SignURL("http://tracker/v1/t/view?tid=t1&cid=c1&exp=9999999999", key)
	u, _ := url.Parse(signed)
	q := u.Query()
	if !adserving.ValidateSignatureAny(u.Path, viewSigParams(q), []string{key}) {
		t.Fatal("no-measurement view URL failed validation")
	}
}

package adcert

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

func sampleReq() *openrtb.BidRequest {
	return &openrtb.BidRequest{
		ID:  "trace-1",
		Imp: []openrtb.Imp{{ID: "imp-1", TagID: "pl-1", BidFloor: 1.25}},
		Site: &openrtb.Site{
			Domain:    "news.example",
			Publisher: &openrtb.Publisher{ID: "pub-1"},
		},
		Source: &openrtb.Source{Ext: &openrtb.SourceExt{SChain: &openrtb.SupplyChain{
			Ver:   openrtb.SChainVersion,
			Nodes: []openrtb.SupplyChainNode{{ASI: "adtech.example", SID: "pub-1", HP: 1}},
		}}},
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	req := sampleReq()
	sig := Sign(priv, req)
	if sig == "" {
		t.Fatal("empty signature")
	}
	if !Verify(pub, req, sig) {
		t.Error("valid signature failed to verify")
	}
}

func TestVerifyFailsOnTamper(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	req := sampleReq()
	sig := Sign(priv, req)

	tampers := []func(*openrtb.BidRequest){
		func(r *openrtb.BidRequest) { r.Imp[0].BidFloor = 99.0 },                   // floor bumped
		func(r *openrtb.BidRequest) { r.Site.Domain = "evil.example" },             // domain swapped
		func(r *openrtb.BidRequest) { r.Imp[0].TagID = "pl-other" },                // placement swapped
		func(r *openrtb.BidRequest) { r.Source.Ext.SChain.Nodes[0].SID = "pub-2" }, // seller swapped
		func(r *openrtb.BidRequest) { r.ID = "trace-2" },                           // request id swapped
		func(r *openrtb.BidRequest) { r.Source.Ext.AdCertTS = 999 },                // timestamp forged
	}
	for i, tamper := range tampers {
		r := sampleReq()
		tamper(r)
		if Verify(pub, r, sig) {
			t.Errorf("tamper %d: signature verified against a modified request", i)
		}
	}
}

func TestFresh(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	withTS := func(ts int64) *openrtb.BidRequest {
		r := sampleReq()
		r.Source.Ext.AdCertTS = ts
		return r
	}

	tests := []struct {
		name   string
		req    *openrtb.BidRequest
		maxAge time.Duration
		want   bool
	}{
		{"fresh (just signed)", withTS(now.Unix()), 5 * time.Minute, true},
		{"within window", withTS(now.Add(-2 * time.Minute).Unix()), 5 * time.Minute, true},
		{"too old", withTS(now.Add(-10 * time.Minute).Unix()), 5 * time.Minute, false},
		{"future skew within window", withTS(now.Add(1 * time.Minute).Unix()), 5 * time.Minute, true},
		{"far future rejected", withTS(now.Add(30 * time.Minute).Unix()), 5 * time.Minute, false},
		{"no timestamp is stale", sampleReq(), 5 * time.Minute, false},
		{"maxAge 0 disables check", withTS(0), 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Fresh(tt.req, now, tt.maxAge); got != tt.want {
				t.Errorf("Fresh = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVerifyFailsWrongKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)
	req := sampleReq()
	sig := Sign(priv, req)
	if Verify(otherPub, req, sig) {
		t.Error("signature verified under the wrong public key")
	}
}

func TestVerifyMalformed(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	req := sampleReq()
	cases := []string{"", "!!!not-base64!!!", "YWJj" /* valid b64, wrong length */}
	for _, sig := range cases {
		if Verify(pub, req, sig) {
			t.Errorf("malformed sig %q verified", sig)
		}
	}
	// nil / wrong-size public key.
	if Verify(nil, req, Sign(mustPriv(t), req)) {
		t.Error("verify succeeded with nil public key")
	}
}

func TestKeyParsingRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)

	privB64 := EncodeKey(priv)
	pubB64 := EncodeKey(pub)

	gotPriv, err := ParsePrivateKey(privB64)
	if err != nil {
		t.Fatalf("parse private: %v", err)
	}
	gotPub, err := ParsePublicKey(pubB64)
	if err != nil {
		t.Fatalf("parse public: %v", err)
	}

	// The parsed keys must produce a verifiable signature.
	req := sampleReq()
	if !Verify(gotPub, req, Sign(gotPriv, req)) {
		t.Error("round-tripped keys failed to sign/verify")
	}

	// Empty → nil, no error (unconfigured = disabled).
	if k, err := ParsePrivateKey(""); k != nil || err != nil {
		t.Errorf("empty private key: got %v, %v", k, err)
	}
	if k, err := ParsePublicKey(""); k != nil || err != nil {
		t.Errorf("empty public key: got %v, %v", k, err)
	}

	// Wrong length → error.
	if _, err := ParsePublicKey(EncodeKey([]byte("too-short"))); err == nil {
		t.Error("expected error for short public key")
	}
}

func TestPublicKeyB64(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)

	// The published key must equal the private key's public half and parse back.
	got := PublicKeyB64(priv)
	if got != EncodeKey(pub) {
		t.Errorf("PublicKeyB64 = %q, want %q", got, EncodeKey(pub))
	}
	parsed, err := ParsePublicKey(got)
	if err != nil {
		t.Fatalf("parse published key: %v", err)
	}
	// And it must verify a signature made with the private key.
	req := sampleReq()
	if !Verify(parsed, req, Sign(priv, req)) {
		t.Error("published key failed to verify a genuine signature")
	}

	if PublicKeyB64(nil) != "" {
		t.Error("nil key should publish as empty")
	}
}

func TestCanonicalStable(t *testing.T) {
	// Canonical must be deterministic for the same request.
	a := Canonical(sampleReq())
	b := Canonical(sampleReq())
	if a != b {
		t.Errorf("canonical not stable:\n%s\n%s", a, b)
	}
	if a == "" {
		t.Error("empty canonical")
	}
}

func mustPriv(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func TestVerifyAny_Overlap(t *testing.T) {
	// Signed with key A. Verifies while the keyset holds [B, A] (overlap window),
	// rejected once A is dropped — the ads.cert rotation grace window.
	pubA, privA, _ := ed25519.GenerateKey(nil)
	pubB, _, _ := ed25519.GenerateKey(nil)
	req := sampleReq()
	sig := Sign(privA, req)
	if !VerifyAny([]ed25519.PublicKey{pubB, pubA}, req, sig) {
		t.Error("expected accept under the overlap keyset [B, A]")
	}
	if VerifyAny([]ed25519.PublicKey{pubB}, req, sig) {
		t.Error("expected reject once key A is dropped from the keyset")
	}
	if VerifyAny(nil, req, sig) {
		t.Error("empty keyset must not verify")
	}
}

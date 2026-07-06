// Package adcert implements ads.cert-style signed bid requests: the exchange
// signs a canonical digest of a bid request's stable fields with an Ed25519
// private key, and the DSP verifies the signature with the exchange's public
// key. This is the authenticity leg of the transparency stack (alongside
// ads.txt / sellers.json / schain) — it proves a bid request actually came
// from the declared exchange and wasn't spoofed or tampered in transit.
//
// Asymmetric (Ed25519) rather than a shared HMAC secret: the DSP only needs the
// exchange's public key, which can be distributed openly — no secret to leak.
//
// Scope note: this signs authenticity over stable request fields. Freshness
// (replay protection via a timestamp/nonce) and public-key distribution via
// published ads.cert files are follow-ups; here the DSP is configured with the
// exchange's public key directly.
package adcert

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// Canonical builds the deterministic byte string that gets signed/verified.
// It covers the stable, security-relevant fields of a request — the identity
// of the inventory and its floor — so tampering with any of them invalidates
// the signature. Field order is fixed; empty fields serialise as empty.
func Canonical(req *openrtb.BidRequest) string {
	var domain, pubID string
	if req.Site != nil {
		domain = req.Site.Domain
		if req.Site.Publisher != nil {
			pubID = req.Site.Publisher.ID
		}
	} else if req.App != nil {
		domain = req.App.Bundle
		if req.App.Publisher != nil {
			pubID = req.App.Publisher.ID
		}
	}
	var tagID string
	var floor float64
	if len(req.Imp) > 0 {
		tagID = req.Imp[0].TagID
		floor = req.Imp[0].BidFloor
	}
	// Include the first schain node so the signed authenticity binds to the
	// declared seller of record.
	var asi, sid string
	if sc := openrtb.SChainOf(req); sc != nil && len(sc.Nodes) > 0 {
		asi = sc.Nodes[0].ASI
		sid = sc.Nodes[0].SID
	}

	var b strings.Builder
	b.WriteString("id=")
	b.WriteString(req.ID)
	b.WriteString("&pub=")
	b.WriteString(pubID)
	b.WriteString("&domain=")
	b.WriteString(domain)
	b.WriteString("&tagid=")
	b.WriteString(tagID)
	b.WriteString("&floor=")
	b.WriteString(strconv.FormatFloat(floor, 'f', 4, 64))
	b.WriteString("&asi=")
	b.WriteString(asi)
	b.WriteString("&sid=")
	b.WriteString(sid)
	return b.String()
}

// Sign returns the base64 (raw-url) Ed25519 signature over Canonical(req).
func Sign(priv ed25519.PrivateKey, req *openrtb.BidRequest) string {
	sig := ed25519.Sign(priv, []byte(Canonical(req)))
	return base64.RawURLEncoding.EncodeToString(sig)
}

// Verify reports whether sig is a valid Ed25519 signature over Canonical(req)
// for the given public key. A malformed signature returns false (never panics).
func Verify(pub ed25519.PublicKey, req *openrtb.BidRequest, sig string) bool {
	if len(pub) != ed25519.PublicKeySize || sig == "" {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, []byte(Canonical(req)), raw)
}

// ParsePrivateKey decodes a base64 (raw-url or std) Ed25519 private key (the
// 64-byte seed+public form). Empty input yields a nil key and no error so
// callers can treat "unconfigured" as "signing disabled".
func ParsePrivateKey(b64 string) (ed25519.PrivateKey, error) {
	if b64 == "" {
		return nil, nil
	}
	raw, err := decodeB64(b64)
	if err != nil {
		return nil, fmt.Errorf("adcert: decode private key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("adcert: private key is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}

// ParsePublicKey decodes a base64 (raw-url or std) Ed25519 public key. Empty
// input yields a nil key and no error ("verification not configured").
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	if b64 == "" {
		return nil, nil
	}
	raw, err := decodeB64(b64)
	if err != nil {
		return nil, fmt.Errorf("adcert: decode public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("adcert: public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// EncodeKey base64 (raw-url) encodes a key for config/storage.
func EncodeKey(key []byte) string {
	return base64.RawURLEncoding.EncodeToString(key)
}

// decodeB64 accepts either raw-url (no padding) or standard base64.
func decodeB64(s string) ([]byte, error) {
	if raw, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.StdEncoding.DecodeString(s)
}

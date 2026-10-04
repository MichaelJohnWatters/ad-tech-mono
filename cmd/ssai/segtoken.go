package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
)

// segPayload is the SENSITIVE server-only metadata for one stitched ad segment.
// It used to ride the client-facing /v1/ssai/seg URL as PLAINTEXT params
// (beacon=, redir=, event=), which leaked the clearing price + advertiser /
// campaign / creative ids + HMAC signatures (all carried in the signed beacon
// URLs) and the real media path to anyone inspecting the stream. These are now
// SEALED into one opaque AES-256-GCM token (?t=<opaque>); the handler opens it
// server-side to fire the beacons + redirect. The remaining URL params
// (session/ad/break/seg) are opaque operational ids — a correlation trace and a
// segment index, exactly what a production SSAI opaque-token URL carries — and
// reveal nothing about price or advertiser.
type segPayload struct {
	Ad      string   `json:"a,omitempty"` // ad trace id — forces the beacon traceparent (ads only)
	Events  []string `json:"e,omitempty"`
	Beacons []string `json:"k,omitempty"` // pre-signed, HMAC-valid tracker URLs (ads only; empty = content)
	Redir   string   `json:"r,omitempty"` // the real media chunk to stream/redirect to (content or ad)
}

// segKey derives the AES-256 key for segment tokens from the active beacon
// signing key — already loaded identically on every stitcher replica from the
// secrets store (SetActiveSigningKey at boot), so any pod can open a token any
// pod minted: stateless and multi-replica safe, with NO new secret and NO shared
// store. Domain-separated from beacon signing by a fixed prefix so the two uses
// of the same base key can't collide.
func segKey() [32]byte {
	return sha256.Sum256([]byte("ssai-seg-token\x00" + adserving.ActiveSigningKey()))
}

func segAEAD() (cipher.AEAD, error) {
	k := segKey()
	blk, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

// sealSegToken encrypts a segPayload into a URL-safe (base64url, unpadded) opaque
// token suitable as a bare query-param value.
func sealSegToken(p segPayload) (string, error) {
	aead, err := segAEAD()
	if err != nil {
		return "", err
	}
	pt, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	// DETERMINISTIC (synthetic) nonce = H(key ‖ plaintext) — so the SAME payload
	// always seals to the SAME token. That's required for the live path: a given
	// segment position must get a STABLE URL across the player's manifest re-polls,
	// or it re-downloads every chunk each poll and playback stutters. GCM's
	// nonce-reuse weakness only bites when the SAME nonce pairs with DIFFERENT
	// plaintext; here identical payload → identical nonce → identical ciphertext
	// (fine), and any different payload → different nonce (safe) — i.e. a standard
	// SIV-style deterministic AEAD.
	k := segKey()
	sum := sha256.Sum256(append(append([]byte("ssai-seg-nonce\x00"), k[:]...), pt...))
	nonce := sum[:aead.NonceSize()]
	ct := aead.Seal(nonce, nonce, pt, nil)
	return base64.RawURLEncoding.EncodeToString(ct), nil
}

// openSegToken reverses sealSegToken. A token minted under a different signing
// key (e.g. after a rotation that dropped the old key) fails authentication and
// returns an error — the handler then 400s that segment rather than serving it.
func openSegToken(tok string) (segPayload, error) {
	var p segPayload
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return p, err
	}
	aead, err := segAEAD()
	if err != nil {
		return p, err
	}
	ns := aead.NonceSize()
	if len(raw) < ns {
		return p, errors.New("ssai seg token too short")
	}
	pt, err := aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(pt, &p); err != nil {
		return p, err
	}
	return p, nil
}

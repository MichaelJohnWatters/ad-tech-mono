package main

import (
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adcert"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
)

// adCertSigner signs outbound bid requests with the exchange's ads.cert Ed25519
// key and publishes the verification keyset. The ACTIVE private key comes from
// the secrets store (purpose adcert_ed25519) so it rotates HOT (Phase I); it's
// held in an atomic.Pointer for lock-free reads on the auction fan-out path and
// swapped live by WatchActive. exchange.adcert_sign_key (env) is a back-compat
// fallback when no secret is configured. The published keyset covers EVERY
// non-revoked key so a DSP can verify against the active AND rotating keys
// during a grace window (overlap).
type adCertSigner struct {
	active atomic.Pointer[ed25519.PrivateKey] // active key from the store (via setActiveKey)
	envKey ed25519.PrivateKey                 // fallback: exchange.adcert_sign_key
	cache  *secrets.Cache                     // source of the public keyset (nil-safe)
	now    func() time.Time
	log    *slog.Logger
}

func newAdCertSigner(cfg *config.Config, cache *secrets.Cache, now func() time.Time, log *slog.Logger) *adCertSigner {
	envKey, err := adcert.ParsePrivateKey(keys.Exchange.AdCertSignKey.Get(cfg))
	if err != nil {
		log.Error("adcert: invalid exchange.adcert_sign_key (fallback disabled)", "error", err)
		envKey = nil
	}
	return &adCertSigner{envKey: envKey, cache: cache, now: now, log: log}
}

// setActiveKey is the WatchActive setter: parse the base64 active key and swap
// it into the atomic ref. A bad value is ignored (the current key stays), so a
// malformed secret can't silently disable signing.
func (s *adCertSigner) setActiveKey(b64 string) {
	priv, err := adcert.ParsePrivateKey(b64)
	if err != nil || priv == nil {
		s.log.Error("adcert: active signing key from store is invalid, keeping current", "error", err)
		return
	}
	s.active.Store(&priv)
	s.log.Info("adcert: active signing key updated from the secrets store")
}

// signingKey returns the active key (store) or the env fallback.
func (s *adCertSigner) signingKey() ed25519.PrivateKey {
	if p := s.active.Load(); p != nil {
		return *p
	}
	return s.envKey
}

// sign stamps Source.Ext.AdCert + AdCertTS. No-op when no key is configured
// (fail-open, matching the other trust gates).
func (s *adCertSigner) sign(req *openrtb.BidRequest) {
	priv := s.signingKey()
	if priv == nil {
		return
	}
	if req.Source == nil {
		req.Source = &openrtb.Source{}
	}
	if req.Source.Ext == nil {
		req.Source.Ext = &openrtb.SourceExt{}
	}
	// Sign the timestamp too (it's in the canonical) so the DSP can reject replays.
	req.Source.Ext.AdCertTS = s.now().Unix()
	req.Source.Ext.AdCert = adcert.Sign(priv, req)
}

// publicKeys returns the base64 public halves of every non-revoked ads.cert key
// (active + rotating from the store, plus the env fallback), deduped. This is
// the keyset the /v1/adcert/key endpoint publishes for DSP overlap verification.
func (s *adCertSigner) publicKeys() []string {
	seen := map[string]bool{}
	var out []string
	add := func(priv ed25519.PrivateKey) {
		if priv == nil {
			return
		}
		if b := adcert.PublicKeyB64(priv); b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	// Active first so it heads the keyset (back-compat single `key` field).
	add(s.signingKey())
	add(s.envKey)
	if s.cache != nil {
		for _, sec := range s.cache.NonRevokedByPurpose(secrets.PurposeAdCertEd25519) {
			if priv, err := adcert.ParsePrivateKey(sec.Value); err == nil {
				add(priv)
			}
		}
	}
	return out
}

// AdCertKey is one entry in the published keyset.
type AdCertKey struct {
	Alg string `json:"alg"` // "Ed25519"
	Key string `json:"key"` // base64 raw-url public key
}

// AdCertKeyResponse is the JSON the exchange publishes at /v1/adcert/key. `keys`
// is the full keyset (JWKS-style) DSPs verify against during a rotation; `key`
// is the active key kept for back-compat with older single-key fetchers.
type AdCertKeyResponse struct {
	Alg  string      `json:"alg"`           // "Ed25519" (back-compat)
	Key  string      `json:"key,omitempty"` // the active key (back-compat)
	Keys []AdCertKey `json:"keys"`          // all currently-valid public keys
}

// adCertKeyHandler serves the exchange's ads.cert public keyset so DSPs can
// verify signatures and pick up rotations. 404 when signing isn't configured.
func adCertKeyHandler(signer *adCertSigner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pubs := signer.publicKeys()
		if len(pubs) == 0 {
			http.Error(w, "adcert signing not configured", http.StatusNotFound)
			return
		}
		keyset := make([]AdCertKey, len(pubs))
		for i, p := range pubs {
			keyset[i] = AdCertKey{Alg: "Ed25519", Key: p}
		}
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		// Public keys — safe to cache briefly; rotations overlap so a 60s stale
		// window can't reject a live signature.
		w.Header().Set("Cache-Control", "public, max-age=60")
		json.NewEncoder(w).Encode(AdCertKeyResponse{Alg: "Ed25519", Key: pubs[0], Keys: keyset})
	}
}

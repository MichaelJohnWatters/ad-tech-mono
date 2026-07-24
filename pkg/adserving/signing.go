package adserving

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
)

// DefaultSigningKey is used in dev mode. In production, loaded from K8s secret.
const DefaultSigningKey = "adtech-dev-signing-key-change-in-prod"

// SignURL generates an HMAC-SHA256 signature for a pixel URL.
// Signs all query parameters except 'sig' itself.
func SignURL(rawURL, signingKey string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	params := u.Query()
	params.Del("sig")

	sig := computeSignature(u.Path, params, signingKey)
	params.Set("sig", sig)
	u.RawQuery = params.Encode()
	return u.String()
}

// ValidateSignature checks that a request's signature is valid.
func ValidateSignature(path string, params url.Values, signingKey string) bool {
	sig := params.Get("sig")
	if sig == "" || sig == "TODO" {
		return false
	}

	// Recompute without sig param
	check := url.Values{}
	for k, v := range params {
		if k != "sig" {
			check[k] = v
		}
	}

	expected := computeSignature(path, check, signingKey)
	return hmac.Equal([]byte(sig), []byte(expected))
}

// ValidateSignatureAny reports whether the request signature is valid under ANY
// of the given keys — the overlap check for HMAC key rotation. During a rotation
// the validator's key set is the active key PLUS the rotating predecessor(s), so
// a pixel signed just before the switch still validates until the old key is
// revoked. Empty keys are skipped. Returns false if none match (or all empty).
func ValidateSignatureAny(path string, params url.Values, keys []string) bool {
	for _, k := range keys {
		if k != "" && ValidateSignature(path, params, k) {
			return true
		}
	}
	return false
}

func computeSignature(path string, params url.Values, key string) string {
	// Sort params for deterministic signing
	var keys []string
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	parts = append(parts, path)
	for _, k := range keys {
		parts = append(parts, k+"="+params.Get(k))
	}

	message := strings.Join(parts, "&")
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

package ssoauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDomainAllowed(t *testing.T) {
	for _, tc := range []struct {
		email   string
		allowed []string
		want    bool
	}{
		{"a@corp.com", []string{"corp.com"}, true},
		{"a@CORP.com", []string{"corp.com"}, true},  // case-insensitive
		{"a@corp.com", []string{"@corp.com"}, true}, // leading @ tolerated
		{"a@corp.com", []string{"other.com", "corp.com"}, true},
		{"a@evil.com", []string{"corp.com"}, false},
		{"a@corp.com", nil, false}, // empty allowlist denies all
		{"a@corp.com", []string{}, false},
		{"no-at-sign", []string{"corp.com"}, false},
	} {
		if got := DomainAllowed(tc.email, tc.allowed); got != tc.want {
			t.Errorf("DomainAllowed(%q,%v)=%v want %v", tc.email, tc.allowed, got, tc.want)
		}
	}
}

// --- fake OIDC IdP: real discovery + JWKS + RS256-signed id_token ---

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid})
	body, _ := json.Marshal(claims)
	signingInput := b64u(hdr) + "." + b64u(body)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + b64u(sig)
}

func jwks(key *rsa.PrivateKey, kid string) map[string]any {
	eb := make([]byte, 8)
	binary.BigEndian.PutUint64(eb, uint64(key.E))
	// trim leading zero bytes of the exponent
	i := 0
	for i < len(eb)-1 && eb[i] == 0 {
		i++
	}
	return map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid,
		"n": b64u(key.N.Bytes()), "e": b64u(eb[i:]),
	}}}
}

// newFakeIdP returns an httptest OIDC provider. idToken is a pointer the test
// swaps per-case so the /token endpoint returns whatever id_token the case builds.
func newFakeIdP(t *testing.T, idToken *string) (*httptest.Server, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	const kid = "test-kid"
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 issuer,
			"authorization_endpoint": issuer + "/auth",
			"token_endpoint":         issuer + "/token",
			"jwks_uri":               issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(jwks(key, kid)) })
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": *idToken})
	})
	srv := httptest.NewServer(mux)
	issuer = srv.URL
	// expose the signer to the test via t-scoped closure
	t.Cleanup(srv.Close)
	signers[srv.URL] = func(claims map[string]any) string { return signRS256(t, key, kid, claims) }
	return srv, issuer
}

var signers = map[string]func(map[string]any) string{}

func TestExchangeVerifiesIDToken(t *testing.T) {
	var idToken string
	srv, issuer := newFakeIdP(t, &idToken)
	sign := signers[srv.URL]
	cfg := Config{Issuer: issuer, ClientID: "client1", ClientSecret: "secret"}
	now := time.Now()
	base := func() map[string]any {
		return map[string]any{
			"iss": issuer, "aud": "client1", "sub": "user-1",
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			"nonce": "n1", "email": "alice@corp.com", "email_verified": true, "name": "Alice",
		}
	}
	ctx := context.Background()

	t.Run("valid id_token → identity", func(t *testing.T) {
		idToken = sign(base())
		id, err := Exchange(ctx, cfg, "http://cb", "code", "n1", "verifier")
		if err != nil {
			t.Fatalf("Exchange: %v", err)
		}
		if id.Email != "alice@corp.com" || !id.EmailVerified || id.Subject != "user-1" || id.Name != "Alice" {
			t.Fatalf("identity=%+v", id)
		}
	})
	t.Run("wrong audience rejected", func(t *testing.T) {
		c := base()
		c["aud"] = "someone-else"
		idToken = sign(c)
		if _, err := Exchange(ctx, cfg, "http://cb", "code", "n1", "verifier"); err == nil {
			t.Error("want error on aud mismatch")
		}
	})
	t.Run("wrong issuer rejected", func(t *testing.T) {
		c := base()
		c["iss"] = "https://evil.example"
		idToken = sign(c)
		if _, err := Exchange(ctx, cfg, "http://cb", "code", "n1", "verifier"); err == nil {
			t.Error("want error on iss mismatch")
		}
	})
	t.Run("expired rejected", func(t *testing.T) {
		c := base()
		c["exp"] = now.Add(-time.Hour).Unix()
		idToken = sign(c)
		if _, err := Exchange(ctx, cfg, "http://cb", "code", "n1", "verifier"); err == nil {
			t.Error("want error on expired token")
		}
	})
	t.Run("nonce mismatch rejected", func(t *testing.T) {
		idToken = sign(base()) // nonce n1
		if _, err := Exchange(ctx, cfg, "http://cb", "code", "DIFFERENT", "verifier"); err == nil {
			t.Error("want error on nonce mismatch")
		}
	})
	t.Run("bad signature rejected", func(t *testing.T) {
		// Sign with a different key than the JWKS advertises.
		other, _ := rsa.GenerateKey(rand.Reader, 2048)
		idToken = signRS256(t, other, "test-kid", base())
		if _, err := Exchange(ctx, cfg, "http://cb", "code", "n1", "verifier"); err == nil {
			t.Error("want error on bad signature")
		}
	})
}

func TestAuthCodeURLCarriesSecurityParams(t *testing.T) {
	var idToken string
	_, issuer := newFakeIdP(t, &idToken)
	cfg := Config{Issuer: issuer, ClientID: "client1", ClientSecret: "secret"}
	u, err := AuthCodeURL(context.Background(), cfg, "http://cb", "state123", "nonce123", "verifier-xyz-123456789")
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	for _, want := range []string{"state=state123", "nonce=nonce123", "code_challenge=", "code_challenge_method=S256", "redirect_uri=http%3A%2F%2Fcb", "client_id=client1"} {
		if !contains(u, want) {
			t.Errorf("auth url missing %q: %s", want, u)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

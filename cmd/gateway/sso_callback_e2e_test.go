//go:build e2e

package main

// Full SSO login round-trip, in-process against real Postgres + an httptest fake
// OIDC IdP. The live-stack e2e can't do this (the in-cluster gateway can't reach a
// host-run IdP), so the callback→Exchange→gate→JIT→session path was only covered in
// disjoint pieces. This drives the REAL ssoCallbackHandler end to end and asserts
// the happy path + every security gate.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssoauth"
	ssoauthpg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssoauth/postgres"
)

const ssoTestKey = "sso-callback-test-signing-key-32b!"

func TestSSOCallbackRoundTrip(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://adtech_app:adtech-app-local@localhost:5432/adtech?sslmode=disable")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	// Superuser conn for the test's OWN reads/cleanups on RLS tables (team_members,
	// sso_configurations) — a bare adtech_app query there returns 0 rows. The
	// handler-under-test keeps using the adtech_app `db` so it exercises real RLS.
	su, err := sql.Open("postgres", "postgres://adtech:adtech-local-dev@localhost:5432/adtech?sslmode=disable")
	if err != nil {
		t.Fatalf("open su db: %v", err)
	}
	defer su.Close()

	// --- fake OIDC IdP (discovery + JWKS + RS256-signed id_token) ---
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	const kid = "cb-kid"
	var issuer, idToken string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/a", "token_endpoint": issuer + "/t", "jwks_uri": issuer + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		eb := make([]byte, 8)
		binary.BigEndian.PutUint64(eb, uint64(key.E))
		i := 0
		for i < len(eb)-1 && eb[i] == 0 {
			i++
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid,
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(eb[i:])}}})
	})
	mux.HandleFunc("/t", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idToken})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	issuer = srv.URL

	signID := func(claims map[string]any) string {
		hdr, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid})
		body, _ := json.Marshal(claims)
		in := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(body)
		sum := sha256.Sum256([]byte(in))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		return in + "." + base64.RawURLEncoding.EncodeToString(sig)
	}

	store := ssoauthpg.New(db)
	log := quietLog()
	handler := ssoCallbackHandler(store, db, ssoTestKey, log)

	// configure enables an account's SSO against the fake IdP (direct store write,
	// so we can use the http test issuer that the config-PUT https-check would reject).
	configure := func(acctID, clientID string, domains []string, role string) {
		if err := store.Upsert(ctx, ssoauth.Config{
			AccountID: acctID, Enabled: true, Issuer: issuer, ClientID: clientID,
			ClientSecret: "secret", AllowedDomains: domains, DefaultRole: role,
		}); err != nil {
			t.Fatalf("configure sso: %v", err)
		}
	}
	// drive builds a signed flow cookie for (account,nonce), sets idToken to the
	// given claims, and runs the callback; returns the recorder.
	drive := func(acctID, clientID string, claims map[string]any) *httptest.ResponseRecorder {
		state, nonce, verifier := "st-"+randHex(), "nonce-"+randHex(), "vrf-"+randHex()
		claims["nonce"] = nonce
		claims["iss"] = issuer
		claims["aud"] = clientID
		idToken = signID(claims)
		payload, _ := json.Marshal(flowState{State: state, Nonce: nonce, Verifier: verifier, Account: acctID})
		req := httptest.NewRequest(http.MethodGet, "/v1/auth/sso/callback?code=abc&state="+state, nil)
		req.AddCookie(&http.Cookie{Name: ssoFlowCookie, Value: signFlow(payload, ssoTestKey)})
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	now := time.Now()
	baseClaims := func(email string, verified bool) map[string]any {
		return map[string]any{"sub": "s-" + randHex(), "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "email": email, "email_verified": verified, "name": "SSO User"}
	}
	hasSession := func(rec *httptest.ResponseRecorder) bool {
		for _, c := range rec.Result().Cookies() {
			if c.Name == middleware.SessionCookieName && c.Value != "" {
				return true
			}
		}
		return false
	}

	// --- happy path: JIT provision + session cookie ---
	adv := mustAccount(t, db, "sso-cb-adv-"+randHex()+"@e2e.local")
	email := "alice-" + randHex() + "@corp.com"
	configure(adv, "client-adv", []string{"corp.com"}, "viewer")
	t.Cleanup(func() {
		su.Exec(`DELETE FROM team_members WHERE email = $1`, email)
		su.Exec(`DELETE FROM sso_configurations WHERE account_id = $1::uuid`, adv)
		su.Exec(`DELETE FROM accounts WHERE id = $1::uuid`, adv)
	})
	rec := drive(adv, "client-adv", baseClaims(email, true))
	if rec.Code != http.StatusSeeOther || !hasSession(rec) {
		t.Fatalf("happy path: code=%d session=%v, want 303 + adtech_session", rec.Code, hasSession(rec))
	}
	var tmRole, tmAcct string
	if err := su.QueryRow(`SELECT role, account_id::text FROM team_members WHERE email=$1`, email).Scan(&tmRole, &tmAcct); err != nil {
		t.Fatalf("JIT team_member not created: %v", err)
	}
	if tmRole != "viewer" || tmAcct != adv {
		t.Errorf("JIT member role=%q acct=%q, want viewer + %s", tmRole, tmAcct, adv)
	}

	// --- gate: email_verified=false → 401 ---
	if rec := drive(adv, "client-adv", baseClaims("unverified-"+randHex()+"@corp.com", false)); rec.Code != http.StatusUnauthorized {
		t.Errorf("email_verified=false: code=%d, want 401", rec.Code)
	}
	// --- gate: domain not in allowlist → 403 ---
	if rec := drive(adv, "client-adv", baseClaims("bob-"+randHex()+"@evil.com", true)); rec.Code != http.StatusForbidden {
		t.Errorf("domain not allowed: code=%d, want 403", rec.Code)
	}
	// --- gate: ineligible account type (staff) → 403 ---
	staff := mustStaffAccount(t, db, "sso-cb-staff-"+randHex()+"@e2e.local")
	configure(staff, "client-staff", []string{"corp.com"}, "viewer")
	t.Cleanup(func() {
		su.Exec(`DELETE FROM sso_configurations WHERE account_id = $1::uuid`, staff)
		su.Exec(`DELETE FROM accounts WHERE id = $1::uuid`, staff)
	})
	if rec := drive(staff, "client-staff", baseClaims("staffer-"+randHex()+"@corp.com", true)); rec.Code != http.StatusForbidden {
		t.Errorf("staff account type: code=%d, want 403 (SSO never provisions staff)", rec.Code)
	}
	// --- gate: cross-account (email belongs to account adv) via account B → 403 ---
	advB := mustAccount(t, db, "sso-cb-advB-"+randHex()+"@e2e.local")
	configure(advB, "client-b", []string{"corp.com"}, "viewer")
	t.Cleanup(func() {
		su.Exec(`DELETE FROM sso_configurations WHERE account_id = $1::uuid`, advB)
		su.Exec(`DELETE FROM accounts WHERE id = $1::uuid`, advB)
	})
	if rec := drive(advB, "client-b", baseClaims(email, true)); rec.Code != http.StatusForbidden {
		t.Errorf("cross-account hijack: code=%d, want 403 (email belongs to another account)", rec.Code)
	}
}

func randHex() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b) // lowercase only — emails get lowercased by Exchange
}

func mustStaffAccount(t *testing.T, db *sql.DB, email string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`INSERT INTO accounts (name, email, type, status) VALUES ($1,$2,'staff','active') RETURNING id::text`, "SSO CB Staff", email).Scan(&id); err != nil {
		t.Fatalf("create staff account: %v", err)
	}
	return id
}

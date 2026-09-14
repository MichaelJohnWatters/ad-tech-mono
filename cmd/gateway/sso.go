package main

// sso.go — per-account OIDC single sign-on (PLAN Phase 11 #110).
//
//	GET /v1/auth/sso/start?account=<id>  — redirect to the account's IdP (auth-code
//	                                       + PKCE + state + nonce). Public.
//	GET /v1/auth/sso/callback            — verify state, exchange code, verify the
//	                                       id_token, gate (email_verified + domain +
//	                                       account type), JIT-provision a least-
//	                                       privilege team_member, mint the SAME
//	                                       session cookie password login mints. Public.
//	GET/PUT /v1/api/account/sso          — owner reads/writes the config (sso:manage).
//
// SSO NEVER provisions privileged account types (staff/admin/partner) and NEVER
// logs a user into an account their email doesn't already belong to.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ssoauth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

const ssoFlowCookie = "adtech_sso_flow"

// ssoAccountTypesAllowed are the only account types SSO may authenticate into —
// privileged types (staff/admin/partner) are provisioned out-of-band, never via SSO.
var ssoAccountTypesAllowed = map[string]bool{"advertiser": true, "publisher": true, "agency": true}

var (
	errSSOCrossAccount = errors.New("email belongs to another account")
	errSSOInactive     = errors.New("team member not active")
)

// flowState is the short-lived, HMAC-signed cookie payload carrying the CSRF/PKCE
// material from /start to /callback (the client never gets to read or forge it).
type flowState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Account  string `json:"a"`
}

func ssoRandToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func signFlow(payload []byte, key string) string {
	p := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func verifyFlow(tok, key string) ([]byte, bool) {
	p, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return nil, false
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(p))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	return b, err == nil
}

// callbackURL derives the registered redirect_uri from the request (scheme+host).
func callbackURL(r *http.Request) string {
	scheme := "http"
	if middleware.RequestIsSecure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + routes.AuthSSOCallback
}

func ssoStartHandler(store ssoauth.Store, signingKey string, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if signingKey == "" || store == nil {
			http.Error(w, "sso unavailable", http.StatusServiceUnavailable)
			return
		}
		account := r.URL.Query().Get("account")
		if !uuidRe.MatchString(account) {
			http.Error(w, "invalid account", http.StatusBadRequest)
			return
		}
		cfg, ok, err := store.ByAccount(r.Context(), account)
		if err != nil {
			log.Error("sso start: config load failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok || !cfg.Enabled {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		state, nonce, verifier := ssoRandToken(), ssoRandToken(), ssoRandToken()
		redirect, err := ssoauth.AuthCodeURL(r.Context(), cfg, callbackURL(r), state, nonce, verifier)
		if err != nil {
			log.Error("sso start: auth url failed", "account", account, "error", err)
			http.Error(w, "sso provider error", http.StatusBadGateway)
			return
		}
		payload, _ := json.Marshal(flowState{State: state, Nonce: nonce, Verifier: verifier, Account: account})
		http.SetCookie(w, &http.Cookie{
			Name: ssoFlowCookie, Value: signFlow(payload, signingKey), Path: routes.AuthSSOCallback,
			HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 600, Secure: middleware.RequestIsSecure(r),
		})
		http.Redirect(w, r, redirect, http.StatusSeeOther)
	}
}

func ssoCallbackHandler(store ssoauth.Store, db *sql.DB, signingKey string, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if signingKey == "" || store == nil || db == nil {
			http.Error(w, "sso unavailable", http.StatusServiceUnavailable)
			return
		}
		// Consume the flow cookie (state/nonce/PKCE) and clear it.
		c, err := r.Cookie(ssoFlowCookie)
		http.SetCookie(w, &http.Cookie{Name: ssoFlowCookie, Value: "", Path: routes.AuthSSOCallback, MaxAge: -1})
		if err != nil {
			http.Error(w, "sso: no flow in progress", http.StatusBadRequest)
			return
		}
		raw, ok := verifyFlow(c.Value, signingKey)
		if !ok {
			http.Error(w, "sso: invalid flow", http.StatusBadRequest)
			return
		}
		var fs flowState
		if err := json.Unmarshal(raw, &fs); err != nil {
			http.Error(w, "sso: invalid flow", http.StatusBadRequest)
			return
		}
		// CSRF: the IdP-returned state must match the one we stashed.
		if s := r.URL.Query().Get("state"); s == "" || s != fs.State {
			http.Error(w, "sso: state mismatch", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "sso: missing code", http.StatusBadRequest)
			return
		}
		cfg, ok, err := store.ByAccount(r.Context(), fs.Account)
		if err != nil || !ok || !cfg.Enabled {
			http.Error(w, "sso: config unavailable", http.StatusBadRequest)
			return
		}
		id, err := ssoauth.Exchange(r.Context(), cfg, callbackURL(r), code, fs.Nonce, fs.Verifier)
		if err != nil {
			log.Warn("sso callback: verification failed", "account", fs.Account, "error", err)
			http.Error(w, "sso: authentication failed", http.StatusUnauthorized)
			return
		}
		if !id.EmailVerified || id.Email == "" {
			http.Error(w, "sso: email not verified", http.StatusUnauthorized)
			return
		}
		if !ssoauth.DomainAllowed(id.Email, cfg.AllowedDomains) {
			log.Warn("sso callback: email domain not allowed", "account", fs.Account)
			http.Error(w, "sso: email domain not permitted", http.StatusForbidden)
			return
		}

		acctType, residency, ok, err := ssoAccount(r.Context(), db, fs.Account)
		if err != nil {
			log.Error("sso callback: account load failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok || !ssoAccountTypesAllowed[acctType] {
			http.Error(w, "sso: account not eligible", http.StatusForbidden)
			return
		}
		tmID, role, err := ssoJITProvision(r.Context(), db, fs.Account, id.Email, id.Name, cfg.DefaultRole)
		if errors.Is(err, errSSOCrossAccount) || errors.Is(err, errSSOInactive) {
			http.Error(w, "sso: user not permitted for this account", http.StatusForbidden)
			return
		}
		if err != nil {
			log.Error("sso callback: provisioning failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Mint the SAME session the password path mints.
		now := time.Now()
		at := auth.AccountType(acctType)
		claims := &auth.Claims{
			UserID:          auth.MintUserID(tmID),
			AccountID:       fs.Account,
			AccountType:     at,
			Role:            auth.Role(role),
			Permissions:     auth.RolePermissions(at, auth.Role(role)),
			ResidencyRegion: residency,
			IssuedAt:        now,
			ExpiresAt:       now.Add(12 * time.Hour),
		}
		token, err := middleware.CreateToken(claims, signingKey)
		if err != nil {
			log.Error("sso callback: token mint failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: middleware.SessionCookieName, Value: token, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: claims.ExpiresAt, Secure: middleware.RequestIsSecure(r),
		})
		log.Info("sso login ok", "account", fs.Account, "account_type", acctType, "role", role)
		http.Redirect(w, r, portalHome(at), http.StatusSeeOther)
	}
}

// ssoAccount reads the account's type + residency (cross-tenant, pre-auth).
func ssoAccount(ctx context.Context, db *sql.DB, accountID string) (acctType, residency string, ok bool, err error) {
	var status string
	err = postgres.NewFromDB(db).QueryRowPlatform(ctx, func(row *sql.Row) error {
		return row.Scan(&acctType, &residency, &status)
	}, `SELECT type, residency_region, status FROM accounts WHERE id = $1::uuid`, accountID)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return acctType, residency, status == "active", nil
}

// ssoJITProvision returns the (team_member id, role) for email under accountID,
// creating a least-privilege member on first login. A team_member whose email
// already belongs to a DIFFERENT account is rejected (no cross-account hijack).
func ssoJITProvision(ctx context.Context, db *sql.DB, accountID, email, name, defaultRole string) (string, string, error) {
	pg := postgres.NewFromDB(db)
	var existingID, existingAcct, existingRole, existingStatus string
	err := pg.QueryRowPlatform(ctx, func(row *sql.Row) error {
		return row.Scan(&existingID, &existingAcct, &existingRole, &existingStatus)
	}, `SELECT id::text, account_id::text, role, status FROM team_members WHERE email = $1`, email)
	switch {
	case err == nil:
		if existingAcct != accountID {
			return "", "", errSSOCrossAccount
		}
		if existingStatus != "active" {
			return "", "", errSSOInactive
		}
		return existingID, existingRole, nil
	case err != sql.ErrNoRows:
		return "", "", err
	}
	if name == "" {
		name = email
	}
	// Defence in depth: never provision above least-privilege even if a bad
	// default_role somehow reached the config row (the PUT validator + the mig-105
	// CHECK both constrain it, but don't trust the stored value at login).
	role := defaultRole
	if !ssoDefaultRoles[role] {
		role = "viewer"
	}
	// JIT create. password_hash is a non-bcrypt sentinel so this user can never
	// password-login. This is a WRITE, so it needs a read-WRITE tenant tx —
	// QueryRowTenantDB opens a read-only tx (INSERT there fails 25006).
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_account_id', $1, true)", accountID); err != nil {
		return "", "", err
	}
	var id string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO team_members (account_id, email, name, role, password_hash, status)
		 VALUES ($1::uuid, $2, $3, $4, 'sso-no-password', 'active') RETURNING id::text`,
		accountID, email, name, role).Scan(&id); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return id, role, nil
}

// --- Config management (owner) ---

type ssoConfigResponse struct {
	Enabled         bool     `json:"enabled"`
	Issuer          string   `json:"issuer"`
	ClientID        string   `json:"client_id"`
	AllowedDomains  []string `json:"allowed_domains"`
	DefaultRole     string   `json:"default_role"`
	HasClientSecret bool     `json:"has_client_secret"`
	StartURL        string   `json:"start_url"`
}

type ssoConfigRequest struct {
	Enabled        bool     `json:"enabled"`
	Issuer         string   `json:"issuer"`
	ClientID       string   `json:"client_id"`
	ClientSecret   string   `json:"client_secret"` // "" on update = keep existing
	AllowedDomains []string `json:"allowed_domains"`
	DefaultRole    string   `json:"default_role"`
}

var ssoDefaultRoles = map[string]bool{"viewer": true, "analyst": true, "ad_ops": true, "finance": true, "manager": true}

func ssoConfigHandler(store ssoauth.Store, auditDB *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if !can(claims, "sso:manage") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if store == nil {
			http.Error(w, `{"error":"sso unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		switch r.Method {
		case http.MethodGet:
			cfg, ok, err := store.ByAccount(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("sso config get failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			resp := ssoConfigResponse{DefaultRole: "viewer", StartURL: routes.AuthSSOStart + "?account=" + claims.AccountID}
			if ok {
				resp = ssoConfigResponse{
					Enabled: cfg.Enabled, Issuer: cfg.Issuer, ClientID: cfg.ClientID,
					AllowedDomains: cfg.AllowedDomains, DefaultRole: cfg.DefaultRole,
					HasClientSecret: cfg.ClientSecret != "", StartURL: routes.AuthSSOStart + "?account=" + claims.AccountID,
				}
			}
			_ = json.NewEncoder(w).Encode(resp)

		case http.MethodPut:
			r.Body = http.MaxBytesReader(w, r.Body, maxSupportBodyBytes)
			var req ssoConfigRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			req.Issuer = strings.TrimSpace(req.Issuer)
			if req.DefaultRole == "" {
				req.DefaultRole = "viewer"
			}
			if !ssoDefaultRoles[req.DefaultRole] {
				http.Error(w, `{"error":"default_role must be a least-privilege role (viewer/analyst/ad_ops/finance/manager)"}`, http.StatusBadRequest)
				return
			}
			// When enabling, the config must be complete + safe: an HTTPS issuer URL
			// (OIDC discovery + token exchange + JWKS carry the client_secret and
			// id_token — cleartext would expose them to a MITM), a client id, and a
			// non-empty domain allowlist (an empty allowlist denies all, so enabling
			// with none is a footgun).
			if req.Enabled {
				if u, err := url.Parse(req.Issuer); err != nil || u.Scheme != "https" || u.Host == "" {
					http.Error(w, `{"error":"issuer must be an https URL"}`, http.StatusBadRequest)
					return
				}
				if strings.TrimSpace(req.ClientID) == "" {
					http.Error(w, `{"error":"client_id required when enabled"}`, http.StatusBadRequest)
					return
				}
				if len(req.AllowedDomains) == 0 {
					http.Error(w, `{"error":"allowed_domains required when enabled"}`, http.StatusBadRequest)
					return
				}
			}
			if req.AllowedDomains == nil {
				req.AllowedDomains = []string{}
			}
			// Cap the allowlist (scanned on every callback; owner-writable) — mirrors
			// the changelog affected_endpoints cap.
			if len(req.AllowedDomains) > 50 {
				http.Error(w, `{"error":"too many allowed_domains (max 50)"}`, http.StatusBadRequest)
				return
			}
			for _, d := range req.AllowedDomains {
				if len(d) > 253 {
					http.Error(w, `{"error":"allowed_domain too long"}`, http.StatusBadRequest)
					return
				}
			}
			if err := store.Upsert(r.Context(), ssoauth.Config{
				AccountID: claims.AccountID, Enabled: req.Enabled, Issuer: req.Issuer, ClientID: req.ClientID,
				ClientSecret: req.ClientSecret, AllowedDomains: req.AllowedDomains, DefaultRole: req.DefaultRole,
			}); err != nil {
				log.Error("sso config upsert failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			if auditDB != nil {
				_ = audit.Log(r.Context(), auditDB, audit.Entry{
					AccountID: claims.AccountID, ActorID: "user:" + claims.UserID, Action: "sso:configure",
					ResourceType: "sso_configuration", ResourceID: claims.AccountID,
					Changes: map[string]any{"enabled": req.Enabled, "issuer": req.Issuer, "default_role": req.DefaultRole},
				})
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "saved"})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

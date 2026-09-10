// Package ssoauth implements per-account OIDC single sign-on (PLAN Phase 11 #110):
// an account configures its own IdP; its users log in via the auth-code flow (PKCE +
// state + nonce) and are JIT-provisioned as team_members at a least-privilege role,
// ALONGSIDE the existing password auth. id_token verification (signature via the
// IdP's JWKS, iss/aud/exp) is delegated to coreos/go-oidc — we do not hand-roll
// OIDC crypto.
package ssoauth

import (
	"context"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config is one account's OIDC SSO configuration.
type Config struct {
	AccountID      string
	Enabled        bool
	Issuer         string
	ClientID       string
	ClientSecret   string // confidential-client secret — NEVER serialized to the config API
	AllowedDomains []string
	DefaultRole    string
}

// Store persists per-account SSO config.
type Store interface {
	// ByAccount returns the account's config. Used by the pre-auth login path
	// (cross-tenant read) and the owner config API. ok=false when no row exists.
	ByAccount(ctx context.Context, accountID string) (Config, bool, error)
	// Upsert writes the account's config (owner, tenant-scoped).
	Upsert(ctx context.Context, cfg Config) error
}

// Identity is the verified subject of a completed OIDC login.
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

var scopes = []string{oidc.ScopeOpenID, "email", "profile"}

// newFlow builds the oauth2 config + id_token verifier for cfg against redirectURL,
// performing IdP discovery (issuer/.well-known/openid-configuration).
func newFlow(ctx context.Context, cfg Config, redirectURL string) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	p, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("oidc discovery: %w", err)
	}
	oa := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  redirectURL,
		Scopes:       scopes,
	}
	return oa, p.Verifier(&oidc.Config{ClientID: cfg.ClientID}), nil
}

// AuthCodeURL builds the IdP redirect URL for the auth-code flow (state for CSRF,
// nonce bound into the id_token, PKCE S256 challenge from pkceVerifier).
func AuthCodeURL(ctx context.Context, cfg Config, redirectURL, state, nonce, pkceVerifier string) (string, error) {
	oa, _, err := newFlow(ctx, cfg, redirectURL)
	if err != nil {
		return "", err
	}
	return oa.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(pkceVerifier)), nil
}

// Exchange completes the flow: swaps the code for tokens (PKCE verifier), verifies
// the id_token (signature via JWKS + iss + aud + exp) and the nonce, and returns the
// verified identity. Every failure is an auth failure — callers must reject.
func Exchange(ctx context.Context, cfg Config, redirectURL, code, nonce, pkceVerifier string) (Identity, error) {
	oa, verifier, err := newFlow(ctx, cfg, redirectURL)
	if err != nil {
		return Identity{}, err
	}
	tok, err := oa.Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return Identity{}, fmt.Errorf("token exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return Identity{}, fmt.Errorf("no id_token in token response")
	}
	idt, err := verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("id_token verify: %w", err)
	}
	if idt.Nonce != nonce {
		return Identity{}, fmt.Errorf("nonce mismatch")
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idt.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("id_token claims: %w", err)
	}
	return Identity{
		Subject:       idt.Subject,
		Email:         strings.ToLower(strings.TrimSpace(claims.Email)),
		EmailVerified: claims.EmailVerified,
		Name:          strings.TrimSpace(claims.Name),
	}, nil
}

// DomainAllowed reports whether email's domain is in the allowlist. An EMPTY
// allowlist denies all: an SSO config must explicitly scope which email domains may
// JIT-provision, so a misconfigured/empty config can never admit arbitrary users.
func DomainAllowed(email string, allowed []string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 || len(allowed) == 0 {
		return false
	}
	dom := strings.ToLower(email[at+1:])
	for _, d := range allowed {
		if strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@")) == dom {
			return true
		}
	}
	return false
}

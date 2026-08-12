package main

// partner.go — staff-facing external-partner registry (PLAN Phase 11, item 112).
//
//	GET   /v1/api/partners           — list partners, optional ?status= filter (partners:read)
//	GET   /v1/api/partners?id=X      — one partner (partners:read)
//	POST  /v1/api/partners           — register (no id) or edit metadata (id present) (partners:manage)
//	POST  /v1/api/partners/status    — {id,status} lifecycle transition (partners:manage)
//
// Partners are platform-global (not tenant-scoped); staff register and shepherd
// them through the onboarding lifecycle (pending -> sandbox -> certified ->
// active). The partner-facing self-serve portal builds on this in a later slice.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/partner"
	partnerpg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/partner/postgres"
)

// partnerEmailRe is a light email sanity check (real validation is delivery).
var partnerEmailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// randToken returns n random bytes as hex (used for a one-time temp password).
// It surfaces a rand failure rather than returning "" — provisioning an account
// with a password derived from empty/zero entropy would be an auth hole.
func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

const maxPartnerNameLen = 200

// partnerUUIDRe validates an id param before it reaches the ::uuid cast, so a
// garbage id is a clean 400 rather than a 500 from Postgres 22P02.
var partnerUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// writePartnerErr maps a store error to a client-facing status. Returns true if
// it handled (wrote) the error.
func writePartnerErr(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, partnerpg.ErrNotFound):
		http.Error(w, `{"error":"partner not found"}`, http.StatusNotFound)
	case errors.Is(err, partnerpg.ErrDuplicateName):
		http.Error(w, `{"error":"a partner with that name already exists"}`, http.StatusConflict)
	case errors.Is(err, partnerpg.ErrConstraint):
		http.Error(w, `{"error":"invalid field value"}`, http.StatusBadRequest)
	default:
		return false
	}
	return true
}

// partnersHandler serves list/get/register/edit on /v1/api/partners.
func partnersHandler(store partner.Store, auditDB *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if store == nil {
			http.Error(w, `{"error":"partners unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "partners:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			if id := strings.TrimSpace(r.URL.Query().Get("id")); id != "" {
				if !partnerUUIDRe.MatchString(id) {
					http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
					return
				}
				p, err := store.Get(r.Context(), id)
				if writePartnerErr(w, err) {
					return
				}
				if err != nil {
					log.Error("partner get failed", "error", err)
					http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
					return
				}
				_ = json.NewEncoder(w).Encode(p)
				return
			}
			statusFilter := strings.TrimSpace(r.URL.Query().Get("status"))
			if statusFilter != "" && !partner.IsValidStatus(statusFilter) {
				http.Error(w, `{"error":"invalid status filter"}`, http.StatusBadRequest)
				return
			}
			list, err := store.List(r.Context(), statusFilter)
			if err != nil {
				log.Error("partner list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(list)

		case http.MethodPost:
			if !can(claims, "partners:manage") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			handlePartnerWrite(w, r, store, auditDB, claims.UserID, log)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

type partnerWriteRequest struct {
	ID string `json:"id,omitempty"`
	partner.Input
}

func handlePartnerWrite(w http.ResponseWriter, r *http.Request, store partner.Store, auditDB *sql.DB, userID string, log *slog.Logger) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSupportBodyBytes)
	var req partnerWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}
	if len(req.Name) > maxPartnerNameLen {
		http.Error(w, `{"error":"name too long"}`, http.StatusBadRequest)
		return
	}
	if req.Kind != "" && !partner.IsValidKind(req.Kind) {
		http.Error(w, `{"error":"kind must be dsp or ssp"}`, http.StatusBadRequest)
		return
	}
	if req.AuthMethod != "" && !partner.IsValidAuthMethod(req.AuthMethod) {
		http.Error(w, `{"error":"auth_method must be api_key, mtls or none"}`, http.StatusBadRequest)
		return
	}

	if req.ID == "" {
		p, err := store.Create(r.Context(), req.Input, userID)
		if writePartnerErr(w, err) {
			return
		}
		if err != nil {
			log.Error("partner create failed", "name", req.Name, "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		auditPartner(r, auditDB, userID, "partner:create", p.ID, p.Name, p.Status)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(p)
		return
	}
	if !partnerUUIDRe.MatchString(req.ID) {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	p, err := store.Update(r.Context(), req.ID, req.Input)
	if writePartnerErr(w, err) {
		return
	}
	if err != nil {
		log.Error("partner update failed", "id", req.ID, "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	auditPartner(r, auditDB, userID, "partner:update", p.ID, p.Name, p.Status)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(p)
}

// partnerStatusHandler serves lifecycle transitions on /v1/api/partners/status.
func partnerStatusHandler(store partner.Store, auditDB *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !can(claims, "partners:manage") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if store == nil {
			http.Error(w, `{"error":"partners unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxSupportBodyBytes)
		var req struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		req.ID = strings.TrimSpace(req.ID)
		req.Status = strings.TrimSpace(req.Status)
		if !partnerUUIDRe.MatchString(req.ID) || !partner.IsValidStatus(req.Status) {
			http.Error(w, `{"error":"a valid id and status are required"}`, http.StatusBadRequest)
			return
		}
		cur, err := store.Get(r.Context(), req.ID)
		if writePartnerErr(w, err) {
			return
		}
		if err != nil {
			log.Error("partner status get failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if cur.Status == req.Status {
			_ = json.NewEncoder(w).Encode(cur) // idempotent no-op
			return
		}
		if !partner.CanTransition(cur.Status, req.Status) {
			http.Error(w, `{"error":"invalid transition from `+cur.Status+` to `+req.Status+`"}`, http.StatusConflict)
			return
		}
		p, err := store.SetStatus(r.Context(), req.ID, req.Status)
		if err != nil {
			log.Error("partner set status failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		auditPartner(r, auditDB, claims.UserID, "partner:status", p.ID, p.Name, p.Status)
		_ = json.NewEncoder(w).Encode(p)
	}
}

// partnerProvisionHandler: POST /v1/api/partners/provision {id, email, name?}
// — staff (partners:manage) create the partner's self-serve login. Returns the
// one-time temp password (never stored/shown again).
func partnerProvisionHandler(store partner.Store, auditDB *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !can(claims, "partners:manage") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if store == nil {
			http.Error(w, `{"error":"partners unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxSupportBodyBytes)
		var req struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Name  string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		req.ID = strings.TrimSpace(req.ID)
		req.Email = strings.TrimSpace(strings.ToLower(req.Email))
		req.Name = strings.TrimSpace(req.Name)
		if !partnerUUIDRe.MatchString(req.ID) || !partnerEmailRe.MatchString(req.Email) {
			http.Error(w, `{"error":"a valid id and email are required"}`, http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			req.Name = req.Email
		}
		temp, err := randToken(9) // 18 hex chars
		if err != nil {
			log.Error("partner provision: rand failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(temp), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		accountID, err := store.ProvisionLogin(r.Context(), req.ID, req.Name, req.Email, string(hash))
		switch {
		case errors.Is(err, partnerpg.ErrNotFound):
			http.Error(w, `{"error":"partner not found"}`, http.StatusNotFound)
			return
		case errors.Is(err, partnerpg.ErrAlreadyProvisioned):
			http.Error(w, `{"error":"this partner already has a login"}`, http.StatusConflict)
			return
		case errors.Is(err, partnerpg.ErrDuplicateName) || errors.Is(err, partnerpg.ErrConstraint):
			http.Error(w, `{"error":"that email is already in use"}`, http.StatusConflict)
			return
		case err != nil:
			log.Error("partner provision failed", "id", req.ID, "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		auditPartner(r, auditDB, claims.UserID, "partner:provision", req.ID, req.Name, "login")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"account_id":    accountID,
			"login_email":   req.Email,
			"temp_password": temp,
			"note":          "share this one-time password with the partner; it is not stored and won't be shown again",
		})
	}
}

// partnerMeHandler: GET /v1/api/partner/me — a provisioned partner reads its own
// registry record (onboarding status, endpoint). Partner accounts only.
func partnerMeHandler(store partner.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if claims.AccountType != auth.AccountPartner || !can(claims, "partner:self") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if store == nil {
			http.Error(w, `{"error":"partners unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		p, err := store.GetByAccount(r.Context(), claims.AccountID)
		if errors.Is(err, partnerpg.ErrNotFound) {
			http.Error(w, `{"error":"no partner record for this account"}`, http.StatusNotFound)
			return
		}
		if err != nil {
			log.Error("partner me failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(p)
	}
}

func auditPartner(r *http.Request, auditDB *sql.DB, userID, action, id, name, status string) {
	if auditDB == nil {
		return
	}
	_ = audit.Log(r.Context(), auditDB, audit.Entry{
		ActorID:      "user:" + userID,
		Action:       action,
		ResourceType: "partner",
		ResourceID:   id,
		Changes:      map[string]any{"name": name, "status": status},
	})
}

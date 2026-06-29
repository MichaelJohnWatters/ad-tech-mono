package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
)

// secretsListResponse is the shape returned to the UI for /v1/api/secrets
// GETs. Value is intentionally masked so we never leak full credential
// material through the read path — operators only see the full value
// once, at POST-time. The mask is a stable prefix the operator can use
// to identify which row they're looking at without exposing the secret.
type secretsListItem struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Purpose      string     `json:"purpose"`
	Owner        string     `json:"owner"`
	Status       string     `json:"status"`
	ValuePreview string     `json:"value_preview"` // first 6 chars + "…"
	RotatedAt    *time.Time `json:"rotated_at,omitempty"`
	RevokesAt    *time.Time `json:"revokes_at,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// secretsCreateRequest accepts an optional value. When absent the
// gateway generates 32 random bytes hex-encoded — that's the right
// default for opaque tokens. Operators only supply a value when they
// have a specific one to insert (e.g. mirroring a partner-issued key).
type secretsCreateRequest struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	Owner   string `json:"owner"`
	Status  string `json:"status,omitempty"` // defaults to "active"
	Value   string `json:"value,omitempty"`
}

// secretsCreateResponse echoes the new row back including the full
// value. This is the ONLY time the full value is returned through the
// API — store it now or lose it. The UI must surface this clearly.
type secretsCreateResponse struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	Owner   string `json:"owner"`
	Status  string `json:"status"`
	Value   string `json:"value"`
	Note    string `json:"note"`
}

type secretsPatchRequest struct {
	Status string `json:"status"`
}

// secretsHandler returns a single http.HandlerFunc that dispatches by
// method + path shape:
//
//	GET    /v1/api/secrets[?service=...&purpose=...&include_revoked=true]  → list
//	POST   /v1/api/secrets                                                  → create
//	PATCH  /v1/api/secrets/{id}                                             → update status
//	DELETE /v1/api/secrets/{id}                                             → delete (revoked-only)
//
// Mutations publish adtech.cache.invalidate.secrets so the warm caches
// on every service refresh within a NATS round-trip. Without the
// publish operators would wait up to 30s for the natural poll.
func secretsHandler(db *sql.DB, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, "secrets store unavailable", http.StatusServiceUnavailable)
			return
		}

		// Collection vs item dispatch. Path-strip removes the prefix and
		// returns "" for collection ("/v1/api/secrets") or "{id}" for item.
		idPart := strings.TrimPrefix(r.URL.Path, "/v1/api/secrets")
		idPart = strings.TrimPrefix(idPart, "/")

		switch {
		case r.Method == http.MethodGet && idPart == "":
			listSecrets(w, r, db, log)
		case r.Method == http.MethodPost && idPart == "":
			createSecret(w, r, db, bus, log)
		case r.Method == http.MethodPatch && idPart != "":
			patchSecret(w, r, db, bus, log, idPart)
		case r.Method == http.MethodDelete && idPart != "":
			deleteSecret(w, r, db, bus, log, idPart)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func listSecrets(w http.ResponseWriter, r *http.Request, db *sql.DB, log *slog.Logger) {
	q := `SELECT id::text, name, value, purpose, owner, status,
	             rotated_at, revokes_at, expires_at, created_at
	      FROM secrets WHERE 1=1`
	args := []any{}
	i := 1

	// ?service= filters owners visible to that service: 'platform' (shared
	// keys every service needs) + the named service's own keys. Mirrors
	// the FilterFor() logic in pkg/secrets.
	if svc := r.URL.Query().Get("service"); svc != "" {
		q += fmt.Sprintf(" AND (owner = 'platform' OR owner = $%d)", i)
		args = append(args, svc)
		i++
	}
	if p := r.URL.Query().Get("purpose"); p != "" {
		q += fmt.Sprintf(" AND purpose = $%d", i)
		args = append(args, p)
		i++
	}
	if r.URL.Query().Get("include_revoked") != "true" {
		q += " AND status != 'revoked'"
	}
	q += " ORDER BY created_at DESC LIMIT 500"

	rows, err := db.QueryContext(r.Context(), q, args...)
	if err != nil {
		log.Error("secrets list query failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []secretsListItem{}
	for rows.Next() {
		var item secretsListItem
		var value string
		var rotatedAt, revokesAt, expiresAt sql.NullTime
		if err := rows.Scan(&item.ID, &item.Name, &value, &item.Purpose,
			&item.Owner, &item.Status,
			&rotatedAt, &revokesAt, &expiresAt, &item.CreatedAt); err != nil {
			log.Error("secrets list scan failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		item.ValuePreview = maskValue(value)
		if rotatedAt.Valid {
			t := rotatedAt.Time
			item.RotatedAt = &t
		}
		if revokesAt.Valid {
			t := revokesAt.Time
			item.RevokesAt = &t
		}
		if expiresAt.Valid {
			t := expiresAt.Time
			item.ExpiresAt = &t
		}
		out = append(out, item)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func createSecret(w http.ResponseWriter, r *http.Request, db *sql.DB, bus events.EventBus, log *slog.Logger) {
	var req secretsCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Purpose == "" {
		http.Error(w, "name and purpose are required", http.StatusBadRequest)
		return
	}
	if req.Owner == "" {
		req.Owner = secrets.OwnerPlatform
	}
	if req.Status == "" {
		req.Status = secrets.StatusActive
	}
	if !isValidStatus(req.Status) {
		http.Error(w, "invalid status (active|rotating|revoked)", http.StatusBadRequest)
		return
	}
	if !isValidPurpose(req.Purpose) {
		http.Error(w, "invalid purpose", http.StatusBadRequest)
		return
	}

	value := req.Value
	if value == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			log.Error("secrets create: rand.Read failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		value = hex.EncodeToString(b)
	}

	const q = `
INSERT INTO secrets (name, value, purpose, owner, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, now(), now())
RETURNING id::text`
	var id string
	if err := db.QueryRowContext(r.Context(), q,
		req.Name, value, req.Purpose, req.Owner, req.Status,
	).Scan(&id); err != nil {
		log.Error("secrets create insert failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	publishInvalidate(r.Context(), bus, log, "create", id)

	log.Info("secrets: created", "id", id, "name", req.Name, "purpose", req.Purpose)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(secretsCreateResponse{
		ID:      id,
		Name:    req.Name,
		Purpose: req.Purpose,
		Owner:   req.Owner,
		Status:  req.Status,
		Value:   value,
		Note:    "store this value securely; it will not be shown again",
	})
}

func patchSecret(w http.ResponseWriter, r *http.Request, db *sql.DB, bus events.EventBus, log *slog.Logger, id string) {
	var req secretsPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !isValidStatus(req.Status) {
		http.Error(w, "invalid status (active|rotating|revoked)", http.StatusBadRequest)
		return
	}

	// Use rotated_at to mark the lifecycle transition timestamp. The
	// column is set on any status change so the UI can show "rotated 3h
	// ago" without having to scan an audit log.
	const q = `UPDATE secrets SET status=$1, rotated_at=now(), updated_at=now()
	           WHERE id=$2 RETURNING name`
	var name string
	if err := db.QueryRowContext(r.Context(), q, req.Status, id).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		log.Error("secrets patch failed", "id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	publishInvalidate(r.Context(), bus, log, "patch", id)

	log.Info("secrets: status updated", "id", id, "name", name, "new_status", req.Status)
	w.WriteHeader(http.StatusNoContent)
}

func deleteSecret(w http.ResponseWriter, r *http.Request, db *sql.DB, bus events.EventBus, log *slog.Logger, id string) {
	// Physical delete is only allowed for revoked rows. Active or rotating
	// secrets must be revoked first — keeps the audit trail intact and
	// prevents accidental nukes of live credentials.
	const q = `DELETE FROM secrets WHERE id=$1 AND status='revoked' RETURNING name`
	var name string
	if err := db.QueryRowContext(r.Context(), q, id).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Either the row doesn't exist, or it's not revoked. The two
			// cases need different responses so the UI can prompt
			// correctly — separate query to disambiguate.
			var status string
			err2 := db.QueryRowContext(r.Context(),
				`SELECT status FROM secrets WHERE id=$1`, id).Scan(&status)
			if errors.Is(err2, sql.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, "secret must be revoked before deletion", http.StatusConflict)
			return
		}
		log.Error("secrets delete failed", "id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	publishInvalidate(r.Context(), bus, log, "delete", id)

	log.Info("secrets: deleted", "id", id, "name", name)
	w.WriteHeader(http.StatusNoContent)
}

// publishInvalidate fans out the cache invalidate on every mutation.
// Bus may be nil if NATS is unreachable at boot — log and continue;
// the 30s warm-cache poll catches the change eventually.
func publishInvalidate(ctx context.Context, bus events.EventBus, log *slog.Logger, op, id string) {
	if bus == nil {
		return
	}
	if err := bus.Publish(ctx, events.SubjectCacheInvalidateSecrets, []byte(`{"id":"`+id+`","op":"`+op+`"}`)); err != nil {
		log.Warn("secrets invalidate publish failed", "op", op, "id", id, "error", err)
	}
}

func maskValue(v string) string {
	const prefix = 6
	if len(v) <= prefix {
		return strings.Repeat("*", len(v))
	}
	return v[:prefix] + "…"
}

func isValidStatus(s string) bool {
	switch s {
	case secrets.StatusActive, secrets.StatusRotating, secrets.StatusRevoked:
		return true
	}
	return false
}

func isValidPurpose(p string) bool {
	switch p {
	case secrets.PurposeJWTSigning, secrets.PurposeHMACTracker,
		secrets.PurposePartnerShared, secrets.PurposeServiceS2S,
		secrets.PurposeAPIKey:
		return true
	}
	return false
}

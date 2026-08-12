package main

// partner_keys.go — a partner's self-serve SANDBOX API key (PLAN Phase 11 #112
// slice 2b). The partner generates the X-API-Key value that will authenticate its
// sandbox integration, rotates it with a grace window, and revokes it — the same
// lifecycle as the advertiser conversion key (conversion_key.go), but scoped to
// the partner's own account and gated on the partner account type.
//
//	GET  /v1/api/partner/sandbox-keys         — list this partner's keys, value masked
//	POST /v1/api/partner/sandbox-keys         — generate / ROTATE (returns the full value once)
//	POST /v1/api/partner/sandbox-keys/revoke  — {id} revoke a key
//
// Strictly account-scoped: every row is stamped account_id = claims.AccountID and
// every read/write filters on it, so a partner can only ever touch its OWN keys.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
)

type sandboxKeyItem struct {
	ID           string     `json:"id"`
	Status       string     `json:"status"` // active | rotating
	ValuePreview string     `json:"value_preview"`
	CreatedAt    time.Time  `json:"created_at"`
	RotatedAt    *time.Time `json:"rotated_at,omitempty"`
}

// partnerGate returns the caller's partner account id, or writes a 401/403 and
// returns ok=false. Sandbox keys are partner-account-only + apikeys:manage.
func partnerGate(w http.ResponseWriter, r *http.Request) (accountID string, ok bool) {
	claims := middleware.ClaimsFromContext(r.Context())
	if claims == nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return "", false
	}
	if claims.AccountType != auth.AccountPartner || !can(claims, "apikeys:manage") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return "", false
	}
	return claims.AccountID, true
}

func partnerSandboxKeysHandler(db *sql.DB, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	cipher, err := secrets.NewCipherFromEnv()
	if err != nil {
		log.Error("sandbox-key handler: invalid encryption key, writing plaintext", "error", err)
		cipher = &secrets.Cipher{}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		accountID, ok := partnerGate(w, r)
		if !ok {
			return
		}
		if db == nil {
			http.Error(w, `{"error":"secrets store unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		switch r.Method {
		case http.MethodGet:
			listSandboxKeys(w, r, db, cipher, accountID, log)
		case http.MethodPost:
			rotateSandboxKey(w, r, db, bus, cipher, accountID, log)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

func listSandboxKeys(w http.ResponseWriter, r *http.Request, db *sql.DB, cipher *secrets.Cipher, accountID string, log *slog.Logger) {
	const q = `SELECT id::text, value, status, created_at, rotated_at
	           FROM secrets
	           WHERE purpose = $1 AND account_id = $2 AND status != 'revoked'
	           ORDER BY created_at DESC`
	rows, err := db.QueryContext(r.Context(), q, secrets.PurposePartnerSandbox, accountID)
	if err != nil {
		log.Error("sandbox-key list failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []sandboxKeyItem{}
	for rows.Next() {
		var it sandboxKeyItem
		var value string
		var rotatedAt sql.NullTime
		if err := rows.Scan(&it.ID, &value, &it.Status, &it.CreatedAt, &rotatedAt); err != nil {
			log.Error("sandbox-key scan failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		plain, derr := cipher.Decrypt(value)
		if derr != nil {
			plain = ""
		}
		it.ValuePreview = maskValue(plain)
		if rotatedAt.Valid {
			t := rotatedAt.Time
			it.RotatedAt = &t
		}
		out = append(out, it)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func rotateSandboxKey(w http.ResponseWriter, r *http.Request, db *sql.DB, bus events.EventBus, cipher *secrets.Cipher, accountID string, log *slog.Logger) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Error("sandbox-key: rand.Read failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	value := "sk_" + hex.EncodeToString(b)
	stored, err := cipher.Encrypt(value)
	if err != nil {
		log.Error("sandbox-key: encrypt failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		log.Error("sandbox-key: begin tx failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	// Demote any active key to 'rotating' (still valid during the grace window),
	// then insert the new active key — atomic, so a validator never sees zero keys.
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE secrets SET status='rotating', rotated_at=now(), updated_at=now()
		 WHERE purpose=$1 AND account_id=$2 AND status='active'`,
		secrets.PurposePartnerSandbox, accountID); err != nil {
		log.Error("sandbox-key: demote active failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	var id string
	if err := tx.QueryRowContext(r.Context(),
		`INSERT INTO secrets (name, value, purpose, owner, account_id, status, created_at, updated_at)
		 VALUES ($1, $2, $3, 'platform', $4, 'active', now(), now())
		 RETURNING id::text`,
		"partner-sandbox-"+accountID, stored, secrets.PurposePartnerSandbox, accountID,
	).Scan(&id); err != nil {
		log.Error("sandbox-key: insert failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Error("sandbox-key: commit failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	publishInvalidate(r.Context(), bus, log, "partner-sandbox-key-rotate", id)
	log.Info("partner sandbox key issued", "id", id, "account_id", accountID)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"id":    id,
		"value": value,
		"note":  "send this as the X-API-Key header on your sandbox calls; it will not be shown again. A previous key still works during the rotation grace window until you revoke it.",
	})
}

// partnerSandboxKeyRevokeHandler: POST /v1/api/partner/sandbox-keys/revoke {id}.
func partnerSandboxKeyRevokeHandler(db *sql.DB, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		accountID, ok := partnerGate(w, r)
		if !ok {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if db == nil {
			http.Error(w, `{"error":"secrets store unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxSupportBodyBytes)
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		req.ID = strings.TrimSpace(req.ID)
		if !partnerUUIDRe.MatchString(req.ID) {
			http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
			return
		}
		// Scoped to this account + purpose: a partner can't revoke another's key.
		res, err := db.ExecContext(r.Context(),
			`UPDATE secrets SET status='revoked', updated_at=now()
			 WHERE id=$1::uuid AND account_id=$2 AND purpose=$3 AND status != 'revoked'`,
			req.ID, accountID, secrets.PurposePartnerSandbox)
		if err != nil {
			log.Error("sandbox-key revoke failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			http.Error(w, `{"error":"key not found"}`, http.StatusNotFound)
			return
		}
		publishInvalidate(r.Context(), bus, log, "partner-sandbox-key-revoke", req.ID)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": req.ID, "status": "revoked"})
	}
}

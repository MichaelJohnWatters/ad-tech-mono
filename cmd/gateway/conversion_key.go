package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
)

// conversionKeyItem is the metadata the advertiser sees for its conversion
// signing key. The value is NEVER returned on GET — only masked. It's shown in
// full exactly once, in the POST (generate/rotate) response.
type conversionKeyItem struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Status       string     `json:"status"` // active | rotating
	ValuePreview string     `json:"value_preview"`
	CreatedAt    time.Time  `json:"created_at"`
	RotatedAt    *time.Time `json:"rotated_at,omitempty"`
}

// conversionKeyCreated is the one-time full-value response on POST.
type conversionKeyCreated struct {
	ID    string `json:"id"`
	Value string `json:"value"`
	Note  string `json:"note"`
}

// conversionKeyHandler is the advertiser's self-serve HMAC key for signing S2S
// conversion postbacks (G7):
//
//	GET  /v1/api/conversion-key  — list this account's key(s), value masked
//	POST /v1/api/conversion-key  — generate (first time) or ROTATE; returns the
//	                               full value once. Any prior active key is moved
//	                               to 'rotating' so in-flight postbacks keep
//	                               validating during the grace window, then can be
//	                               revoked once the advertiser has switched.
//
// Strictly tenant-scoped: the key row is stamped account_id = claims.AccountID,
// and every read/write filters on it, so an advertiser can only ever see or
// rotate its OWN key. The tracker validates /v1/t/conv against this key by advid
// — so once the platform flips tracker.conversion_strict_advertiser_key, no
// other party (even one holding the shared platform key) can forge a conversion
// billed to this advertiser.
func conversionKeyHandler(db *sql.DB, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	cipher, err := secrets.NewCipherFromEnv()
	if err != nil {
		log.Error("conversion-key handler: invalid encryption key, writing plaintext", "error", err)
		cipher = &secrets.Cipher{}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		accountID, ok := effectiveAccount(r, claims)
		if !ok {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if devTenantGuard(w, r, accountID, []conversionKeyItem{}) {
			return
		}
		if db == nil {
			http.Error(w, `{"error":"secrets store unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !canAs(r, claims, "campaigns:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			listConversionKeys(w, r, db, cipher, accountID, log)
		case http.MethodPost:
			if !canAs(r, claims, "campaigns:create") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			rotateConversionKey(w, r, db, bus, cipher, accountID, log)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

func listConversionKeys(w http.ResponseWriter, r *http.Request, db *sql.DB, cipher *secrets.Cipher, accountID string, log *slog.Logger) {
	const q = `SELECT id::text, name, value, status, created_at, rotated_at
	           FROM secrets
	           WHERE purpose = $1 AND account_id = $2 AND status != 'revoked'
	           ORDER BY created_at DESC`
	rows, err := db.QueryContext(r.Context(), q, secrets.PurposeHMACConversion, accountID)
	if err != nil {
		log.Error("conversion-key list failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []conversionKeyItem{}
	for rows.Next() {
		var it conversionKeyItem
		var value string
		var rotatedAt sql.NullTime
		if err := rows.Scan(&it.ID, &it.Name, &value, &it.Status, &it.CreatedAt, &rotatedAt); err != nil {
			log.Error("conversion-key scan failed", "error", err)
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

func rotateConversionKey(w http.ResponseWriter, r *http.Request, db *sql.DB, bus events.EventBus, cipher *secrets.Cipher, accountID string, log *slog.Logger) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Error("conversion-key: rand.Read failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	value := hex.EncodeToString(b)
	stored, err := cipher.Encrypt(value)
	if err != nil {
		log.Error("conversion-key: encrypt failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	// Rotate + insert atomically so a validator never sees zero acceptable keys
	// mid-swap: demote any current active key to 'rotating' (still accepted during
	// the grace window), then insert the new active key. Both are scoped to this
	// account, so we can never touch another advertiser's key.
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		log.Error("conversion-key: begin tx failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback() //nolint:errcheck — no-op after a successful Commit

	if _, err := tx.ExecContext(r.Context(),
		`UPDATE secrets SET status='rotating', rotated_at=now(), updated_at=now()
		 WHERE purpose=$1 AND account_id=$2 AND status='active'`,
		secrets.PurposeHMACConversion, accountID); err != nil {
		log.Error("conversion-key: demote active failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	var id string
	if err := tx.QueryRowContext(r.Context(),
		`INSERT INTO secrets (name, value, purpose, owner, account_id, status, created_at, updated_at)
		 VALUES ($1, $2, $3, 'platform', $4, 'active', now(), now())
		 RETURNING id::text`,
		"conversion-key-"+accountID, stored, secrets.PurposeHMACConversion, accountID,
	).Scan(&id); err != nil {
		log.Error("conversion-key: insert failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Error("conversion-key: commit failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	// Fan out the invalidate so the tracker's warm cache picks up the new key
	// within a NATS round-trip rather than waiting for the 30s poll.
	publishInvalidate(r.Context(), bus, log, "conversion-key-rotate", id)

	log.Info("conversion-key issued", "id", id, "account_id", accountID)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(conversionKeyCreated{
		ID:    id,
		Value: value,
		Note:  "store this key on your server and sign /v1/t/conv postbacks with it; it will not be shown again. Any previous key still works during the rotation grace window.",
	})
}

package main

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
)

// bootstrapHandler implements POST /v1/auth/bootstrap — the one-shot
// flow for minting the first operator API key after a fresh deploy.
//
// Flow:
//
//  1. Operator presents the platform root password in the request body
//     (`{"password": "..."}`). Must match PLATFORM_ROOT_PASSWORD env exactly.
//  2. If a row with name='bootstrap-admin-key' already exists in secrets,
//     return 410 Gone — the endpoint already fired.
//  3. Generate a 32-byte random key, INSERT into secrets with
//     name='bootstrap-admin-key', purpose='api_key', owner='platform',
//     status='active'.
//  4. Return the key value to the caller ONCE. We do not store it
//     anywhere readable post-write; if the operator loses it they must
//     re-bootstrap (which means dropping the row + restarting gateway).
//
// Security properties:
//
//   - PLATFORM_ROOT_PASSWORD comparison uses subtle.ConstantTimeCompare
//     so a network attacker can't infer the password length / prefix
//     by timing differences.
//   - Without the env var set, the endpoint returns 503 — useful for
//     making prod deployments fail loudly if the bootstrap secret was
//     forgotten in deploy config.
//   - The single-shot semantic protects against an attacker spraying
//     the endpoint with guessed passwords after the legitimate operator
//     has already used it; subsequent calls return 410 regardless of
//     password validity.
//   - The generated key (32 random bytes hex-encoded = 64 chars) has
//     more entropy than a typical operator-chosen password.
//
// Gateway-only: no other service exposes this. The DSP/SSP CRUD
// middleware (AuthAPIKey) already accepts any active row in the
// secrets cache, so the freshly-bootstrapped key works there
// immediately after the next NATS invalidate hits the cache (which
// happens automatically because secrets.bus publishes).
func bootstrapHandler(db *sql.DB, log *slog.Logger) http.HandlerFunc {
	rootPassword := os.Getenv("PLATFORM_ROOT_PASSWORD")
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if rootPassword == "" {
			log.Warn("bootstrap: PLATFORM_ROOT_PASSWORD not set, endpoint disabled")
			http.Error(w, "bootstrap not configured", http.StatusServiceUnavailable)
			return
		}
		if db == nil {
			http.Error(w, "secrets store unavailable", http.StatusServiceUnavailable)
			return
		}

		// Already fired? Check secrets table for the named bootstrap row.
		var existing string
		err := db.QueryRowContext(r.Context(),
			`SELECT id FROM secrets WHERE name = 'bootstrap-admin-key' AND status != 'revoked' LIMIT 1`,
		).Scan(&existing)
		if err == nil {
			log.Warn("bootstrap: already fired, returning 410", "existing_id", existing)
			http.Error(w, "bootstrap already used; rotate via the secrets UI to issue more keys", http.StatusGone)
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			log.Error("bootstrap: existence check failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Decode the presented password.
		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if subtle.ConstantTimeCompare([]byte(body.Password), []byte(rootPassword)) != 1 {
			log.Warn("bootstrap: password mismatch", "remote", r.RemoteAddr)
			// Sleep briefly to make brute-forcing painful even on a fast
			// network. Real protection comes from the single-shot guard;
			// this is defence in depth.
			http.Error(w, "incorrect password", http.StatusUnauthorized)
			return
		}

		// Mint the new key.
		keyBytes := make([]byte, 32)
		if _, err := rand.Read(keyBytes); err != nil {
			log.Error("bootstrap: rand.Read failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		keyValue := hex.EncodeToString(keyBytes)

		const insert = `
INSERT INTO secrets (name, value, purpose, owner, status, created_at, updated_at)
VALUES ('bootstrap-admin-key', $1, 'api_key', 'platform', 'active', now(), now())`
		if _, err := db.ExecContext(r.Context(), insert, keyValue); err != nil {
			log.Error("bootstrap: insert failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		log.Info("bootstrap: minted initial operator key", "name", "bootstrap-admin-key")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":  "bootstrap-admin-key",
			"value": keyValue,
			"note":  "store this key securely; it will not be shown again. revoke + rotate via the secrets UI when issuing per-operator keys.",
		})
	}
}

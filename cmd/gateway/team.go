package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"golang.org/x/crypto/bcrypt"
)

// teamMemberView is a team row as the UI sees it (no password material).
type teamMemberView struct {
	ID     string `json:"id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

// teamStore is the persistence the team endpoint needs — interface for testing.
type teamStore interface {
	ListTeam(ctx context.Context, accountID string) ([]teamMemberView, error)
	CreateTeamMember(ctx context.Context, accountID, email, name, role, passwordHash string) (id string, err error)
}

var validTeamRoles = map[string]bool{
	"owner": true, "manager": true, "analyst": true, "finance": true, "viewer": true, "ad_ops": true,
}

// teamHandler lists (GET) and invites (POST) team members, always scoped to the
// caller's own account from their JWT claims. GET needs team:read, POST needs
// team:invite (or a "*" superuser). Invite creates the member with a generated
// temporary password, returned once so the inviter can share it.
func teamHandler(store teamStore, log *slog.Logger) http.HandlerFunc {
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
		if devTenantGuard(w, r, accountID, []teamMemberView{}) {
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !canAs(r, claims, "team:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			members, err := store.ListTeam(r.Context(), accountID)
			if err != nil {
				log.Error("team list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(members)

		case http.MethodPost:
			if !canAs(r, claims, "team:invite") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var req struct{ Email, Name, Role string }
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if req.Email == "" || req.Name == "" {
				http.Error(w, `{"error":"email and name required"}`, http.StatusBadRequest)
				return
			}
			if req.Role == "" {
				req.Role = "viewer"
			}
			if !validTeamRoles[req.Role] {
				http.Error(w, `{"error":"invalid role"}`, http.StatusBadRequest)
				return
			}
			tempPassword := randomToken()
			hash, err := bcrypt.GenerateFromPassword([]byte(tempPassword), bcrypt.DefaultCost)
			if err != nil {
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			id, err := store.CreateTeamMember(r.Context(), accountID, req.Email, req.Name, req.Role, string(hash))
			if err != nil {
				log.Error("team invite failed", "error", err)
				http.Error(w, `{"error":"internal error (email may already exist)"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id": id, "email": req.Email, "role": req.Role, "temp_password": tempPassword,
			})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

func can(c *auth.Claims, perm string) bool {
	return auth.HasPermission(c, perm) || auth.HasPermission(c, "*")
}

func randomToken() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pgTeamStore is the Postgres-backed teamStore.
type pgTeamStore struct{ db *sql.DB }

func (s pgTeamStore) ListTeam(ctx context.Context, accountID string) ([]teamMemberView, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	// Tenant GUC must be set or RLS silently blanks the rows under the
	// NOBYPASSRLS app role (security #77).
	rows, closeFn, err := postgres.QueryTenantDB(ctx, s.db, accountID,
		`SELECT id::text, email, name, role, status FROM team_members WHERE account_id = $1::uuid ORDER BY created_at`, accountID)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	out := []teamMemberView{}
	for rows.Next() {
		var m teamMemberView
		if err := rows.Scan(&m.ID, &m.Email, &m.Name, &m.Role, &m.Status); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s pgTeamStore) CreateTeamMember(ctx context.Context, accountID, email, name, role, passwordHash string) (string, error) {
	if s.db == nil {
		return "", sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return "", fmt.Errorf("set tenant: %w", err)
	}
	var id string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO team_members (account_id, email, name, role, password_hash, status, created_at, updated_at)
		 VALUES ($1::uuid, $2, $3, $4, $5, 'active', now(), now()) RETURNING id::text`,
		accountID, email, name, role, passwordHash).Scan(&id); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"golang.org/x/crypto/bcrypt"
)

// signupInput is a self-serve account registration.
type signupInput struct {
	Name        string
	Email       string
	Password    string
	AccountType string // advertiser | publisher (self-serve only)
}

// signupStore is the persistence signup needs — an interface so the handler is
// testable without a database.
type signupStore interface {
	EmailTaken(ctx context.Context, email string) (bool, error)
	CreateAccountWithOwner(ctx context.Context, in signupInput, passwordHash string) (accountID, userID string, err error)
}

// signupHandler registers an advertiser/publisher account + its owner user,
// then logs them straight in (session cookie) and lands them on their portal.
func signupHandler(store signupStore, signingKey string, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		in := signupInput{
			Name:        strings.TrimSpace(r.FormValue("name")),
			Email:       strings.TrimSpace(strings.ToLower(r.FormValue("email"))),
			Password:    r.FormValue("password"),
			AccountType: r.FormValue("account_type"),
		}
		if in.Name == "" || in.Email == "" || in.Password == "" {
			http.Error(w, "name, email and password are required", http.StatusBadRequest)
			return
		}
		if len(in.Password) < 8 {
			http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
			return
		}
		if in.AccountType != string(auth.AccountAdvertiser) && in.AccountType != string(auth.AccountPublisher) {
			http.Error(w, "account_type must be advertiser or publisher", http.StatusBadRequest)
			return
		}

		taken, err := store.EmailTaken(r.Context(), in.Email)
		if err != nil {
			log.Error("signup: email check failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if taken {
			http.Error(w, "an account with that email already exists", http.StatusConflict)
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
		if err != nil {
			log.Error("signup: hash failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		accountID, userID, err := store.CreateAccountWithOwner(r.Context(), in, string(hash))
		if err != nil {
			log.Error("signup: create failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		now := time.Now()
		at := auth.AccountType(in.AccountType)
		claims := &auth.Claims{
			UserID:      "user-" + userID,
			AccountID:   accountID,
			AccountType: at,
			Role:        auth.RoleOwner,
			Permissions: auth.RolePermissions(at, auth.RoleOwner),
			IssuedAt:    now,
			ExpiresAt:   now.Add(12 * time.Hour),
		}
		token, err := middleware.CreateToken(claims, signingKey)
		if err != nil {
			log.Error("signup: token failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: middleware.SessionCookieName, Value: token, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: claims.ExpiresAt, Secure: middleware.RequestIsSecure(r),
		})
		log.Info("signup ok", "account_type", at, "account_id", accountID)
		http.Redirect(w, r, portalHome(at), http.StatusSeeOther)
	}
}

// pgSignupStore is the Postgres-backed signupStore.
type pgSignupStore struct{ db *sql.DB }

func (s pgSignupStore) EmailTaken(ctx context.Context, email string) (bool, error) {
	if s.db == nil {
		return false, sql.ErrConnDone
	}
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM accounts WHERE email = $1 UNION SELECT 1 FROM team_members WHERE email = $1 LIMIT 1`, email).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s pgSignupStore) CreateAccountWithOwner(ctx context.Context, in signupInput, passwordHash string) (string, string, error) {
	if s.db == nil {
		return "", "", sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()

	var accountID string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO accounts (name, email, type, status, created_at, updated_at)
		 VALUES ($1, $2, $3, 'active', now(), now()) RETURNING id::text`,
		in.Name, in.Email, in.AccountType).Scan(&accountID); err != nil {
		return "", "", fmt.Errorf("insert account: %w", err)
	}
	// Set tenant so RLS on team_members admits the owner insert.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return "", "", fmt.Errorf("set tenant: %w", err)
	}
	var userID string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO team_members (account_id, email, name, role, password_hash, status, created_at, updated_at)
		 VALUES ($1, $2, $3, 'owner', $4, 'active', now(), now()) RETURNING id::text`,
		accountID, in.Email, in.Name, passwordHash).Scan(&userID); err != nil {
		return "", "", fmt.Errorf("insert owner: %w", err)
	}
	return accountID, userID, tx.Commit()
}

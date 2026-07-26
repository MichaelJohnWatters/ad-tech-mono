package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// Agency → managed-advertiser assignments. Staff assign which advertiser
// accounts an agency may act on behalf of; the agency's session loads these
// into Claims.ManagedAccounts at login, and the gateway's act-as forwards the
// selected managed account as the effective tenant.

// errNotAdvertiser → 400: the managed account must be an advertiser (agencies
// manage the buy side).
var errNotAdvertiser = errors.New("managed account must be an advertiser account")

// errNotAgency → 400: the agency_account_id must be an agency account.
var errNotAgency = errors.New("agency_account_id must be an agency account")

type agencyManagedView struct {
	AgencyID    string `json:"agency_id"`
	AgencyName  string `json:"agency_name"`
	ManagedID   string `json:"managed_account_id"`
	ManagedName string `json:"managed_account_name"`
}

type agencyAssignInput struct {
	AgencyAccountID  string `json:"agency_account_id"`
	ManagedAccountID string `json:"managed_account_id"`
}

type agencyAccountStore interface {
	// ListManagedAccounts returns assignments; agencyID="" returns all (staff).
	ListManagedAccounts(ctx context.Context, agencyID string) ([]agencyManagedView, error)
	AssignManagedAccount(ctx context.Context, agencyID, managedID string) error
	UnassignManagedAccount(ctx context.Context, agencyID, managedID string) error
}

// agencyAccountsHandler lists (GET), assigns (POST, staff), and unassigns
// (DELETE, staff) agency→managed-advertiser links. An agency session (agency:read)
// sees only its own managed accounts — that's the data behind the act-as switcher.
func agencyAccountsHandler(store agencyAccountStore, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		isStaff := auth.IsPlatformUser(claims) || can(claims, "*")

		switch r.Method {
		case http.MethodGet:
			// An agency sees its own; staff see all (or a specific ?agency_id).
			var forAgency string
			switch {
			case claims.AccountType == auth.AccountAgency && can(claims, "agency:read"):
				forAgency = claims.AccountID
			case isStaff:
				forAgency = r.URL.Query().Get("agency_id") // "" = all
			default:
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			list, err := store.ListManagedAccounts(r.Context(), forAgency)
			if err != nil {
				log.Error("agency accounts list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(list)

		case http.MethodPost:
			if !isStaff {
				http.Error(w, `{"error":"forbidden: staff only"}`, http.StatusForbidden)
				return
			}
			var in agencyAssignInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if !uuidRe.MatchString(in.AgencyAccountID) || !uuidRe.MatchString(in.ManagedAccountID) {
				http.Error(w, `{"error":"agency_account_id and managed_account_id must be UUIDs"}`, http.StatusBadRequest)
				return
			}
			err := store.AssignManagedAccount(r.Context(), in.AgencyAccountID, in.ManagedAccountID)
			if errors.Is(err, errNotAdvertiser) || errors.Is(err, errNotAgency) {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			if err != nil {
				log.Error("agency assign failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "assigned"})

		case http.MethodDelete:
			if !isStaff {
				http.Error(w, `{"error":"forbidden: staff only"}`, http.StatusForbidden)
				return
			}
			agencyID := r.URL.Query().Get("agency_id")
			managedID := r.URL.Query().Get("managed_account_id")
			if !uuidRe.MatchString(agencyID) || !uuidRe.MatchString(managedID) {
				http.Error(w, `{"error":"agency_id and managed_account_id query params must be UUIDs"}`, http.StatusBadRequest)
				return
			}
			if err := store.UnassignManagedAccount(r.Context(), agencyID, managedID); err != nil {
				log.Error("agency unassign failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "unassigned"})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

type pgAgencyAccountStore struct{ db *sql.DB }

func (s pgAgencyAccountStore) ListManagedAccounts(ctx context.Context, agencyID string) ([]agencyManagedView, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	q := `SELECT ama.agency_account_id::text, ag.name, ama.managed_account_id::text, mg.name
	      FROM agency_managed_accounts ama
	      JOIN accounts ag ON ag.id = ama.agency_account_id
	      JOIN accounts mg ON mg.id = ama.managed_account_id`
	var args []any
	if agencyID != "" {
		q += ` WHERE ama.agency_account_id = $1::uuid`
		args = append(args, agencyID)
	}
	q += ` ORDER BY ag.name, mg.name`
	// Cross-account JOIN (agency ↔ its managed accounts), already scoped by the
	// agency_account_id filter → platform hatch so RLS admits both sides under
	// the NOBYPASSRLS app role (security #77).
	rows, closeFn, err := postgres.NewFromDB(s.db).QueryPlatform(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	out := []agencyManagedView{}
	for rows.Next() {
		var v agencyManagedView
		if err := rows.Scan(&v.AgencyID, &v.AgencyName, &v.ManagedID, &v.ManagedName); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s pgAgencyAccountStore) AssignManagedAccount(ctx context.Context, agencyID, managedID string) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Both ends must be the right account type.
	var agencyType, managedType string
	if err := tx.QueryRowContext(ctx, `SELECT type FROM accounts WHERE id = $1::uuid`, agencyID).Scan(&agencyType); err != nil {
		if err == sql.ErrNoRows {
			return errNotAgency
		}
		return err
	}
	if agencyType != string(auth.AccountAgency) {
		return errNotAgency
	}
	if err := tx.QueryRowContext(ctx, `SELECT type FROM accounts WHERE id = $1::uuid`, managedID).Scan(&managedType); err != nil {
		if err == sql.ErrNoRows {
			return errNotAdvertiser
		}
		return err
	}
	if managedType != string(auth.AccountAdvertiser) {
		return errNotAdvertiser
	}
	// Tenant = the agency so the RLS policy admits the insert.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, agencyID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agency_managed_accounts (agency_account_id, managed_account_id, created_at)
		 VALUES ($1::uuid, $2::uuid, now()) ON CONFLICT DO NOTHING`, agencyID, managedID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s pgAgencyAccountStore) UnassignManagedAccount(ctx context.Context, agencyID, managedID string) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, agencyID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM agency_managed_accounts WHERE agency_account_id = $1::uuid AND managed_account_id = $2::uuid`,
		agencyID, managedID); err != nil {
		return err
	}
	return tx.Commit()
}

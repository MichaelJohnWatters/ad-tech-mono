package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reportrunner"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// savedReportView is one saved_reports row as the reports console sees it.
type savedReportView struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	QueryConfig json.RawMessage `json:"query_config"`
	Schedule    string          `json:"schedule,omitempty"`
	Delivery    string          `json:"delivery"`
}

type savedReportInput struct {
	Name        string          `json:"name"`
	QueryConfig json.RawMessage `json:"query_config"`
	Schedule    string          `json:"schedule"` // cron expression, or "" for manual
	Delivery    string          `json:"delivery"` // email | webhook | none
}

type savedReportStore interface {
	ListSavedReports(ctx context.Context, accountID string) ([]savedReportView, error)
	CreateSavedReport(ctx context.Context, accountID string, in savedReportInput) (id string, err error)
	// DeleteSavedReport removes a report owned by accountID; sql.ErrNoRows if
	// none matches for that tenant.
	DeleteSavedReport(ctx context.Context, accountID, id string) error
}

// validDeliveries mirrors the saved_reports.delivery convention.
var validDeliveries = map[string]bool{"email": true, "webhook": true, "none": true}

// savedReportsHandler manages a caller's saved/scheduled reports: GET lists
// (reports:read), POST creates (reports:save), DELETE removes (reports:save).
// Tenant-scoped — every query filters by the caller's account.
func savedReportsHandler(store savedReportStore, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, []savedReportView{}) {
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "reports:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			reports, err := store.ListSavedReports(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("saved report list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(reports)

		case http.MethodPost:
			if !can(claims, "reports:save") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var in savedReportInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			in.Name = strings.TrimSpace(in.Name)
			if in.Name == "" {
				http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
				return
			}
			if len(in.QueryConfig) == 0 || !json.Valid(in.QueryConfig) {
				http.Error(w, `{"error":"query_config must be a valid JSON object"}`, http.StatusBadRequest)
				return
			}
			if in.Delivery == "" {
				in.Delivery = "none"
			}
			if !validDeliveries[in.Delivery] {
				http.Error(w, `{"error":"delivery must be email, webhook or none"}`, http.StatusBadRequest)
				return
			}
			// A junk schedule would save fine and then silently never run —
			// validate here so the caller learns immediately. Accepted:
			// interval keywords (@hourly/@daily/@weekly/@monthly) or 5-field
			// cron expressions ("30 6 * * 1").
			if strings.TrimSpace(in.Schedule) != "" && !reportrunner.ValidSchedule(in.Schedule) {
				http.Error(w, `{"error":"schedule must be @hourly/@daily/@weekly/@monthly or a 5-field cron expression"}`, http.StatusBadRequest)
				return
			}
			id, err := store.CreateSavedReport(r.Context(), claims.AccountID, in)
			if err != nil {
				log.Error("saved report create failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "name": in.Name})

		case http.MethodDelete:
			if !can(claims, "reports:save") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			id := strings.TrimSpace(r.URL.Query().Get("id"))
			if id == "" {
				http.Error(w, `{"error":"id query param required"}`, http.StatusBadRequest)
				return
			}
			err := store.DeleteSavedReport(r.Context(), claims.AccountID, id)
			if err == sql.ErrNoRows {
				http.Error(w, `{"error":"report not found"}`, http.StatusNotFound)
				return
			}
			if err != nil {
				log.Error("saved report delete failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "deleted"})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

type pgSavedReportStore struct{ db *sql.DB }

func (s pgSavedReportStore) ListSavedReports(ctx context.Context, accountID string) ([]savedReportView, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	// Tenant GUC must be set or RLS silently blanks the rows under the
	// NOBYPASSRLS app role (security #77).
	rows, closeFn, err := postgres.QueryTenantDB(ctx, s.db, accountID,
		`SELECT id::text, name, query_config, COALESCE(schedule,''), COALESCE(delivery,'none')
		 FROM saved_reports WHERE account_id = $1::uuid ORDER BY created_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	out := []savedReportView{}
	for rows.Next() {
		var v savedReportView
		if err := rows.Scan(&v.ID, &v.Name, &v.QueryConfig, &v.Schedule, &v.Delivery); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s pgSavedReportStore) CreateSavedReport(ctx context.Context, accountID string, in savedReportInput) (string, error) {
	if s.db == nil {
		return "", sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return "", err
	}
	var scheduleArg any
	if strings.TrimSpace(in.Schedule) != "" {
		scheduleArg = in.Schedule
	}
	var id string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO saved_reports (account_id, name, query_config, schedule, delivery)
		 VALUES ($1::uuid, $2, $3, $4, $5) RETURNING id::text`,
		accountID, in.Name, []byte(in.QueryConfig), scheduleArg, in.Delivery).Scan(&id); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s pgSavedReportStore) DeleteSavedReport(ctx context.Context, accountID, id string) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	res, err := postgres.ExecTenantDB(ctx, s.db, accountID,
		`DELETE FROM saved_reports WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

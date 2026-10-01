package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reportjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// reportJobInput is a job submission: either a saved-report template
// (saved_report_id) or ad-hoc (name + query_config), plus the artifact format
// and optional email delivery.
type reportJobInput struct {
	SavedReportID string          `json:"saved_report_id"`
	Name          string          `json:"name"`
	QueryConfig   json.RawMessage `json:"query_config"`
	Format        string          `json:"format"`
	Delivery      string          `json:"delivery"` // email | none ("" = none)
}

// reportJobStore is what the handlers need from the job queue plus the two
// lookups a submission resolves (template row, email recipient).
type reportJobStore interface {
	Enqueue(ctx context.Context, j reportjobs.Job) (string, error)
	ListByAccount(ctx context.Context, accountID string, limit int) ([]reportjobs.Job, error)
	GetByAccount(ctx context.Context, accountID, id string) (*reportjobs.Job, error)
	// SavedReportForJob loads a template owned by the account; sql.ErrNoRows
	// when absent for that tenant.
	SavedReportForJob(ctx context.Context, accountID, id string) (name string, queryConfig []byte, format string, err error)
	// OwnerEmail resolves the delivery recipient (account owner, same rule as
	// the scheduler). "" when the account has no active members.
	OwnerEmail(ctx context.Context, accountID string) (string, error)
	// SegmentOwned reports whether the segment belongs to the account — the
	// tenancy check for segment-export jobs.
	SegmentOwned(ctx context.Context, accountID, segmentID string) (bool, error)
}

// effectiveReportAccount resolves which account a report-jobs request acts
// for: the session account, or — for a platform user / agency with a valid
// act-as target — the impersonated account (same rule as enforceReportTenant).
// Thin alias for the shared effectiveAccount helper so the act-as rule lives in
// exactly one place.
func effectiveReportAccount(r *http.Request, claims *auth.Claims) (string, bool) {
	return effectiveAccount(r, claims)
}

// reportJobsHandler serves the collection: GET list (reports:read), POST
// submit (reports:export). Tenant-scoped; scope filters are resolved at
// enqueue via reportjobs.ResolveTenantFilters — the same rules the sync query
// proxy enforces in enforceReportTenant — so the worker never re-derives
// tenancy.
func reportJobsHandler(store reportJobStore, scope reportjobs.ScopeLookup, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		accountID, ok := effectiveReportAccount(r, claims)
		if !ok {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if devTenantGuard(w, r, accountID, []reportjobs.Job{}) {
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "reports:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			jobs, err := store.ListByAccount(r.Context(), accountID, 50)
			if err != nil {
				log.Error("report job list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(jobs)

		case http.MethodPost:
			if !can(claims, "reports:export") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var in reportJobInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			job, errMsg, err := buildJob(r.Context(), store, scope, accountID, claims, in)
			if err != nil {
				log.Error("report job build failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			if errMsg != "" {
				http.Error(w, `{"error":"`+errMsg+`"}`, http.StatusBadRequest)
				return
			}
			id, err := store.Enqueue(r.Context(), *job)
			if err != nil {
				log.Error("report job enqueue failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": reportjobs.StatusQueued})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// buildJob validates a submission and assembles the queued Job. It returns
// (nil, message, nil) for client errors and (nil, "", err) for server errors.
func buildJob(ctx context.Context, store reportJobStore, scope reportjobs.ScopeLookup,
	accountID string, claims *auth.Claims, in reportJobInput) (*reportjobs.Job, string, error) {

	name := strings.TrimSpace(in.Name)
	queryConfig := in.QueryConfig
	format := in.Format

	if in.SavedReportID != "" {
		tName, tConfig, tFormat, err := store.SavedReportForJob(ctx, accountID, in.SavedReportID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "saved report not found", nil
		}
		if err != nil {
			return nil, "", err
		}
		name, queryConfig = tName, tConfig
		if format == "" {
			format = tFormat
		}
	}
	if name == "" {
		return nil, "name required", nil
	}
	if format == "" {
		format = reportjobs.FormatCSV
	}
	if !reportjobs.ValidFormat(format) {
		return nil, "format must be csv, json or parquet", nil
	}

	var params analytics.QueryParams
	if len(queryConfig) == 0 || json.Unmarshal(queryConfig, &params) != nil {
		return nil, "query_config must be a valid query object", nil
	}
	if params.Table == "" {
		return nil, "query_config.table required", nil
	}
	// Saved queries store a relative range (range_days) instead of absolute
	// times — the sync console translates it browser-side; jobs resolve it
	// here so a template job isn't silently all-time.
	if params.TimeFrom.IsZero() {
		var rel struct {
			RangeDays int `json:"range_days"`
		}
		if json.Unmarshal(queryConfig, &rel) == nil && rel.RangeDays > 0 {
			params.TimeFrom = time.Now().AddDate(0, 0, -rel.RangeDays)
		}
	}

	if params.Table == reportjobs.TableSegmentMembers {
		// Segment export: ownership IS the tenancy check (the worker's query
		// re-filters by account) — the analytics scope rules don't apply.
		segID := strings.TrimSpace(params.Filters["segment_id"])
		if segID == "" {
			return nil, "segment_members export requires filters.segment_id", nil
		}
		owned, err := store.SegmentOwned(ctx, accountID, segID)
		if err != nil {
			return nil, "", err
		}
		if !owned {
			return nil, "segment not found", nil
		}
	} else {
		filters, err := reportjobs.ResolveTenantFilters(ctx, scope, accountID, params.Filters)
		if err != nil {
			// Scope failures are the caller's to fix (e.g. multi-publisher account
			// with no publisher_id filter) — surface the reason.
			return nil, err.Error(), nil
		}
		params.Filters = filters
	}

	delivery := in.Delivery
	if delivery == "" {
		delivery = reportjobs.DeliveryNone
	}
	if delivery != reportjobs.DeliveryNone && delivery != reportjobs.DeliveryEmail && delivery != reportjobs.DeliveryWebhook {
		return nil, "delivery must be email, webhook or none", nil
	}
	recipient := ""
	if delivery == reportjobs.DeliveryEmail {
		var err error
		recipient, err = store.OwnerEmail(ctx, accountID)
		if err != nil {
			return nil, "", err
		}
		if recipient == "" {
			return nil, "no email recipient on the account", nil
		}
	}

	requestedBy := ""
	if uuidRe.MatchString(claims.UserID) {
		requestedBy = claims.UserID
	}
	return &reportjobs.Job{
		AccountID:     accountID,
		SavedReportID: in.SavedReportID,
		Name:          name,
		QueryConfig:   params,
		Format:        format,
		Delivery:      delivery,
		Recipient:     recipient,
		Source:        reportjobs.SourceManual,
		RequestedBy:   requestedBy,
		// ExpiresAt zero → the report_jobs SQL default (30 days).
	}, "", nil
}

// reportJobByIDHandler serves the subtree: GET {id} status (reports:read) and
// GET {id}/download (reports:export, streamed — the artifact bucket is
// private, so the gateway is the only way an artifact leaves the platform).
func reportJobByIDHandler(store reportJobStore, objStore objects.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		accountID, ok := effectiveReportAccount(r, claims)
		if !ok {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if devTenantGuard(w, r, accountID, map[string]string{}) {
			return
		}

		rest := strings.TrimPrefix(r.URL.Path, routes.APIReportJobs+"/")
		id, sub, _ := strings.Cut(rest, "/")
		if id == "" || (sub != "" && sub != "download") {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		perm := "reports:read"
		if sub == "download" {
			perm = "reports:export"
		}
		if !can(claims, perm) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}

		job, err := store.GetByAccount(r.Context(), accountID, id)
		if err != nil {
			log.Error("report job get failed", "job", id, "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if job == nil {
			http.Error(w, `{"error":"job not found"}`, http.StatusNotFound)
			return
		}

		if sub == "" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(job)
			return
		}

		// Download.
		if job.Status != reportjobs.StatusDone || job.ArtifactKey == "" {
			http.Error(w, `{"error":"job has no artifact (status `+job.Status+`)"}`, http.StatusConflict)
			return
		}
		fw, err := reportjobs.WriterFor(job.Format)
		if err != nil {
			log.Error("report job artifact format unknown", "job", id, "format", job.Format)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		rc, err := objStore.Get(r.Context(), job.ArtifactBucket, job.ArtifactKey)
		if err != nil {
			log.Error("report job artifact fetch failed", "job", id, "key", job.ArtifactKey, "error", err)
			http.Error(w, `{"error":"artifact unavailable"}`, http.StatusBadGateway)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", fw.ContentType())
		w.Header().Set("Content-Disposition", `attachment; filename="`+artifactFilename(job.Name, fw.Ext())+`"`)
		if _, err := io.Copy(w, rc); err != nil {
			log.Error("report job artifact stream failed", "job", id, "error", err)
		}
	}
}

// artifactFilename builds a safe download filename from the job name.
func artifactFilename(name, ext string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r == ' ':
			return '-'
		}
		return -1
	}, name)
	if safe == "" {
		safe = "report"
	}
	return safe + "." + ext
}

// pgReportJobStore adds the two gateway-side lookups to the shared Postgres
// job queue.
type pgReportJobStore struct {
	reportjobs.PostgresJobStore
	db *sql.DB
}

func newPGReportJobStore(db *sql.DB) pgReportJobStore {
	return pgReportJobStore{PostgresJobStore: reportjobs.NewPostgresJobStore(db), db: db}
}

func (s pgReportJobStore) SavedReportForJob(ctx context.Context, accountID, id string) (string, []byte, string, error) {
	if s.db == nil {
		return "", nil, "", sql.ErrConnDone
	}
	var name, format string
	var config []byte
	err := postgres.QueryRowTenantDB(ctx, s.db, accountID, func(row *sql.Row) error {
		return row.Scan(&name, &config, &format)
	}, `SELECT name, query_config, COALESCE(format, 'csv') FROM saved_reports
		 WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID)
	return name, config, format, err
}

// SegmentOwned is the segment-export tenancy check.
func (s pgReportJobStore) SegmentOwned(ctx context.Context, accountID, segmentID string) (bool, error) {
	var one int
	err := postgres.QueryRowTenantDB(ctx, s.db, accountID, func(row *sql.Row) error {
		return row.Scan(&one)
	}, `SELECT 1 FROM audience_segments WHERE id = $1::uuid AND account_id = $2::uuid`,
		segmentID, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// OwnerEmail mirrors the scheduler's recipient rule (pkg/reportrunner store):
// prefer the owner role, else the earliest active member.
func (s pgReportJobStore) OwnerEmail(ctx context.Context, accountID string) (string, error) {
	if s.db == nil {
		return "", sql.ErrConnDone
	}
	var email string
	err := postgres.QueryRowTenantDB(ctx, s.db, accountID, func(row *sql.Row) error {
		return row.Scan(&email)
	}, `SELECT COALESCE((SELECT tm.email FROM team_members tm
		         WHERE tm.account_id = $1::uuid AND tm.status = 'active'
		         ORDER BY (tm.role = 'owner') DESC, tm.created_at LIMIT 1), '')`, accountID)
	return email, err
}

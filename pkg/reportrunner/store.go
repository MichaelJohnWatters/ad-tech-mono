package reportrunner

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// PostgresStore reads schedulable reports across all tenants (a platform job,
// like the ads.txt crawler). The dev Postgres role bypasses RLS; a prod
// deployment needs a service role permitted to read every saved_reports row.
type PostgresStore struct{ DB *sql.DB }

// NewPostgresStore returns a Store backed by db.
func NewPostgresStore(db *sql.DB) PostgresStore { return PostgresStore{DB: db} }

// ScheduledReports returns every email-delivery report with a non-empty
// schedule, joined with the account owner's email (recipient).
func (s PostgresStore) ScheduledReports(ctx context.Context) ([]ScheduledReport, error) {
	const q = `
SELECT sr.id::text, sr.account_id::text, sr.name, sr.query_config::text,
       COALESCE(sr.schedule, ''), COALESCE(sr.delivery, 'none'), sr.last_run_at,
       COALESCE((SELECT tm.email FROM team_members tm
                 WHERE tm.account_id = sr.account_id AND tm.status = 'active'
                 ORDER BY (tm.role = 'owner') DESC, tm.created_at LIMIT 1), '')
FROM saved_reports sr
WHERE sr.schedule IS NOT NULL AND sr.schedule <> '' AND sr.delivery = 'email'`
	rows, err := s.DB.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledReport
	for rows.Next() {
		var r ScheduledReport
		var queryJSON string
		var lastRun sql.NullTime
		if err := rows.Scan(&r.ID, &r.AccountID, &r.Name, &queryJSON,
			&r.Schedule, &r.Delivery, &lastRun, &r.Recipient); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(queryJSON), &r.QueryConfig); err != nil {
			return nil, fmt.Errorf("decode query_config for %s: %w", r.ID, err)
		}
		if lastRun.Valid {
			t := lastRun.Time
			r.LastRun = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkRun stamps last_run_at for a report.
func (s PostgresStore) MarkRun(ctx context.Context, id string, t time.Time) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE saved_reports SET last_run_at = $2, updated_at = now() WHERE id = $1::uuid`, id, t)
	return err
}

// HTTPQuery posts a report's query to the reporting service. account_id is
// already forced into params.Filters by the runner; the header is set too for
// tracing/consistency.
func HTTPQuery(reportingURL string, client *http.Client) QueryFunc {
	return func(ctx context.Context, accountID string, params analytics.QueryParams) (analytics.QueryResult, error) {
		body, err := json.Marshal(params)
		if err != nil {
			return analytics.QueryResult{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, reportingURL+routes.ReportingQuery, bytes.NewReader(body))
		if err != nil {
			return analytics.QueryResult{}, err
		}
		req.Header.Set(constants.HeaderContentType, constants.ContentTypeJSON)
		req.Header.Set(constants.HeaderAccountID, accountID)
		resp, err := client.Do(req)
		if err != nil {
			return analytics.QueryResult{}, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return analytics.QueryResult{}, fmt.Errorf("reporting query status %d", resp.StatusCode)
		}
		var out analytics.QueryResult
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return analytics.QueryResult{}, err
		}
		return out, nil
	}
}

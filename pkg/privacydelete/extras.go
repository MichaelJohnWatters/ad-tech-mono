package privacydelete

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// LakePurger purges the user-keyed Delta lake tables by calling the
// pipeline's purge endpoints — the pipeline is the lake's single writer, so
// the filtered rewrite must run in its process, not ours. BaseURL is the
// pipeline service (e.g. routes.DefaultPipelineURL or http://pipeline:8087).
type LakePurger struct {
	BaseURL string
	Client  *http.Client
}

func (l *LakePurger) client() *http.Client {
	if l.Client != nil {
		return l.Client
	}
	return &http.Client{Timeout: 60 * time.Second} // rewrite reads every active file
}

// PurgeExtra POSTs the purge and maps the pipeline's table counts onto the
// lake system names.
func (l *LakePurger) PurgeExtra(ctx context.Context, userID string) (map[string]int, error) {
	body, _ := json.Marshal(map[string]string{"user_id": userID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.BaseURL+routes.DatalakePurge, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	counts, err := l.do(req)
	if err != nil {
		return nil, fmt.Errorf("lake purge: %w", err)
	}
	return map[string]int{
		SystemLakeProfileSignals:   counts["profile_signals"],
		SystemLakeBehaviourSignals: counts["behaviour_signals"],
	}, nil
}

// ResidualExtra returns the lake systems still holding rows for the user.
func (l *LakePurger) ResidualExtra(ctx context.Context, userID string) ([]string, error) {
	u := l.BaseURL + routes.DatalakeResidual + "?user_id=" + url.QueryEscape(userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	counts, err := l.do(req)
	if err != nil {
		return nil, fmt.Errorf("lake residual: %w", err)
	}
	var residual []string
	if counts["profile_signals"] > 0 {
		residual = append(residual, SystemLakeProfileSignals)
	}
	if counts["behaviour_signals"] > 0 {
		residual = append(residual, SystemLakeBehaviourSignals)
	}
	return residual, nil
}

func (l *LakePurger) do(req *http.Request) (map[string]int, error) {
	resp, err := l.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pipeline %s: %s", resp.Status, string(body))
	}
	var counts map[string]int
	if err := json.Unmarshal(body, &counts); err != nil {
		return nil, fmt.Errorf("decode pipeline response: %w", err)
	}
	return counts, nil
}

// SignalsStore is the analytics seam for purging the ClickHouse profile-store
// tables (behaviour_signals + profile_signals) and re-deriving the affected
// Parquet export partitions. Implemented by pkg/store/analytics.ClickHouse.
type SignalsStore interface {
	PurgeUserSignals(ctx context.Context, userID string) error
	CountUserSignals(ctx context.Context, userID string) (behaviour, profile int, err error)
	AffectedSignalHours(ctx context.Context, userID string) ([]time.Time, error)
	ExportHourToParquet(ctx context.Context, cfg analytics.ExportConfig, hour time.Time) (map[string]int64, error)
}

// SignalsPurger closes the ClickHouse profile-store purge gap opened in ADR
// 0006: phase 1 landed behaviour_signals/profile_signals in ClickHouse and
// phase 4 exports them to Parquet, but neither copy was covered by Level-3
// deletion. It deletes the ClickHouse rows and then re-exports the exact hours
// the user appeared in, so the derived Parquet archive loses them too. The
// export config is nil-valued (Endpoint == "") when the archive isn't
// configured — then only the ClickHouse delete runs (still correct; the export
// re-derives from ClickHouse on its next scheduled run).
type SignalsPurger struct {
	Store     SignalsStore
	ExportCfg analytics.ExportConfig
	Log       *slog.Logger
}

func (s *SignalsPurger) PurgeExtra(ctx context.Context, userID string) (map[string]int, error) {
	beh, prof, err := s.Store.CountUserSignals(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("signals count: %w", err)
	}
	// Capture affected hours BEFORE the delete removes the rows.
	var hours []time.Time
	if s.ExportCfg.Endpoint != "" {
		if hours, err = s.Store.AffectedSignalHours(ctx, userID); err != nil {
			return nil, fmt.Errorf("signals affected hours: %w", err)
		}
	}
	if err := s.Store.PurgeUserSignals(ctx, userID); err != nil {
		return nil, fmt.Errorf("signals purge: %w", err)
	}
	// Re-derive each affected Parquet partition so the archive loses the user.
	// A failed re-export is a residual-archive alert, not a silent pass.
	for _, h := range hours {
		if _, err := s.Store.ExportHourToParquet(ctx, s.ExportCfg, h); err != nil {
			return nil, fmt.Errorf("signals re-export hour %s: %w", h.Format(time.RFC3339), err)
		}
	}
	if s.Log != nil && len(hours) > 0 {
		s.Log.Info("gdpr: re-exported affected parquet partitions after signal delete",
			"user", userID, "hours", len(hours))
	}
	return map[string]int{
		SystemCHBehaviourSignals: beh,
		SystemCHProfileSignals:   prof,
	}, nil
}

func (s *SignalsPurger) ResidualExtra(ctx context.Context, userID string) ([]string, error) {
	beh, prof, err := s.Store.CountUserSignals(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("signals residual: %w", err)
	}
	var residual []string
	if beh > 0 {
		residual = append(residual, SystemCHBehaviourSignals)
	}
	if prof > 0 {
		residual = append(residual, SystemCHProfileSignals)
	}
	return residual, nil
}

// FreqCapStore is the analytics seam for the freq_cap_blocks purge —
// implemented by pkg/store/analytics.ClickHouse. (The table only exists on
// the ClickHouse backend; memory/duckdb deployments don't wire this purger.)
type FreqCapStore interface {
	PurgeFreqCapBlocks(ctx context.Context, userID string) error
	CountFreqCapBlocks(ctx context.Context, userID string) (int, error)
}

// FreqCapPurger closes the freq_cap_blocks purge gap: the table carries
// user_id (it exists to explain WHY a serve was suppressed) and was never
// covered by Level-3 deletion.
type FreqCapPurger struct{ Store FreqCapStore }

// PurgeExtra deletes the user's freq-cap block rows. ClickHouse lightweight
// DELETE doesn't report affected rows, so the count is taken before.
func (f *FreqCapPurger) PurgeExtra(ctx context.Context, userID string) (map[string]int, error) {
	n, err := f.Store.CountFreqCapBlocks(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("freq_cap_blocks count: %w", err)
	}
	if err := f.Store.PurgeFreqCapBlocks(ctx, userID); err != nil {
		return nil, fmt.Errorf("freq_cap_blocks purge: %w", err)
	}
	return map[string]int{SystemFreqCapBlocks: n}, nil
}

func (f *FreqCapPurger) ResidualExtra(ctx context.Context, userID string) ([]string, error) {
	n, err := f.Store.CountFreqCapBlocks(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("freq_cap_blocks residual: %w", err)
	}
	if n > 0 {
		return []string{SystemFreqCapBlocks}, nil
	}
	return nil, nil
}

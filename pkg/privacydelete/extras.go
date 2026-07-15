package privacydelete

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
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

//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// AttributionTouchpoint is one row of the multi-touch breakdown the reporting
// API returns for a conversion.
type AttributionTouchpoint struct {
	TraceID        string  `json:"trace_id"`
	Type           string  `json:"type"`
	At             string  `json:"at"`
	CreditFraction float64 `json:"credit_fraction"`
	CreditRevenue  float64 `json:"credit_revenue"`
}

// AttributionBreakdown is the reporting API's multi-touch response.
type AttributionBreakdown struct {
	ConversionTraceID string                  `json:"conversion_trace_id"`
	Model             string                  `json:"model"`
	Revenue           float64                 `json:"revenue"`
	Touchpoints       []AttributionTouchpoint `json:"touchpoints"`
}

// GetAttribution reads the multi-touch breakdown for a conversion under a model
// from the live reporting API (/v1/reporting/attribution).
func (h *Harness) GetAttribution(t *testing.T, conversionTrace, model string) AttributionBreakdown {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s%s?conversion_trace=%s&model=%s", h.URLs.Reporting, routes.ReportingAttribution, conversionTrace, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("attribution request: %v", err)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("attribution GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("attribution GET status %d: %s", resp.StatusCode, body)
	}
	var out AttributionBreakdown
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("attribution decode: %v (body: %s)", err, body)
	}
	return out
}

// FireConversionAttributedUser is FireConversionAttributed that also carries the
// advertiser account + visitor id, so a click-through conversion can have its
// ASSISTING exposures resolved and recorded in the multi-touch chain.
func (h *Harness) FireConversionAttributedUser(t *testing.T, convTrace, attributedTrace, accountID, uid, convType, currency string, revenue float64) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/t/conv?tid=%s&ctid=%s&type=%s&rev=%.4f&cur=%s&advid=%s&uid=%s",
		h.URLs.Tracker, convTrace, attributedTrace, convType, revenue, currency, accountID, uid)
	h.fireAndConsume(t, url, "conversion")
}

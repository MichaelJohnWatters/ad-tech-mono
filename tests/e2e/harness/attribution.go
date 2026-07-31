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

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
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

// GetAttribution reads the multi-touch breakdown as STAFF (unscoped) — the
// default for tests that just want the data.
func (h *Harness) GetAttribution(t *testing.T, conversionTrace, model string) AttributionBreakdown {
	return h.GetAttributionAs(t, conversionTrace, model, string(auth.AccountStaff), "")
}

// GetAttributionAs reads the breakdown as a given account (type + id) by setting
// the X-Account-* headers the gateway would inject — so tests can exercise the
// reporting-side tenant scope directly. Fails on non-200.
func (h *Harness) GetAttributionAs(t *testing.T, conversionTrace, model, acctType, acctID string) AttributionBreakdown {
	t.Helper()
	resp, body := h.getAttributionRaw(t, conversionTrace, model, acctType, acctID)
	if resp != http.StatusOK {
		t.Fatalf("attribution GET status %d: %s", resp, body)
	}
	var out AttributionBreakdown
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("attribution decode: %v (body: %s)", err, body)
	}
	return out
}

// AttributionStatusAs returns just the HTTP status for a scoped request (for
// asserting forbidden). acctType/acctID may be empty to send no scope headers.
func (h *Harness) AttributionStatusAs(t *testing.T, conversionTrace, acctType, acctID string) int {
	t.Helper()
	status, _ := h.getAttributionRaw(t, conversionTrace, "linear", acctType, acctID)
	return status
}

func (h *Harness) getAttributionRaw(t *testing.T, conversionTrace, model, acctType, acctID string) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s%s?conversion_trace=%s&model=%s", h.URLs.Reporting, routes.ReportingAttribution, conversionTrace, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("attribution request: %v", err)
	}
	if acctType != "" {
		req.Header.Set(constants.HeaderAccountType, acctType)
	}
	if acctID != "" {
		req.Header.Set(constants.HeaderAccountID, acctID)
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		t.Fatalf("attribution GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
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

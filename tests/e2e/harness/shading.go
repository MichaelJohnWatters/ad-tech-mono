//go:build e2e

package harness

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// SetLineItemShading flips a line item's bid-shading mode (disabled /
// conservative / moderate / aggressive), RLS-scoped to the account. The caller
// must RefreshAllCaches afterwards for the DSP warm cache to pick it up.
func (h *Harness) SetLineItemShading(t *testing.T, lineItemID, accountID, mode string) {
	t.Helper()
	h.WithTenant(t, accountID, func(tx *sql.Tx) {
		if _, err := tx.Exec(`UPDATE line_items SET shading_mode = $1, updated_at = now() WHERE id = $2`, mode, lineItemID); err != nil {
			t.Fatalf("set shading_mode=%s: %v", mode, err)
		}
	})
}

// SetLineItemBaseBid updates a line item's base_bid (RLS-scoped). The caller must
// RefreshAllCaches afterwards. Used to make a campaign bid well above the pooled
// placement clearing so its aggressive-shaded bids actually find room to shade.
func (h *Harness) SetLineItemBaseBid(t *testing.T, lineItemID, accountID string, bid float64) {
	t.Helper()
	h.WithTenant(t, accountID, func(tx *sql.Tx) {
		if _, err := tx.Exec(`UPDATE line_items SET base_bid = $1, updated_at = now() WHERE id = $2`, bid, lineItemID); err != nil {
			t.Fatalf("set base_bid=%v: %v", bid, err)
		}
	})
}

// ClickHouseFloat runs a scalar float-returning query against ClickHouse. The
// integer-only ClickHouseScalar rounds sub-dollar sums (e.g. a few cents of
// bid-shading saving) to 0, so money assertions need the float value.
func (h *Harness) ClickHouseFloat(t *testing.T, query string) float64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := h.clickhouseQuery(ctx, query)
	if err != nil {
		t.Fatalf("clickhouse float query: %v", err)
	}
	var f float64
	_, _ = fmt.Sscanf(strings.TrimSpace(out), "%g", &f)
	return f
}

//go:build e2e

package harness

import (
	"database/sql"
	"testing"
	"time"
)

// SetDealStatus flips a deal's status (active / paused / draft / ended).
// Used by paused-deal tests; the deals warm cache filters on status='active'
// at load time, so callers should RefreshAllCaches after this.
func (h *Harness) SetDealStatus(t *testing.T, dealID, accountID, status string) {
	t.Helper()
	h.WithTenant(t, accountID, func(tx *sql.Tx) {
		if _, err := tx.Exec("UPDATE deals SET status = $1, updated_at = now() WHERE id = $2", status, dealID); err != nil {
			t.Fatalf("set deal status: %v", err)
		}
	})
}

// SetDealDates updates start_date and/or end_date. Pass a nil pointer for
// fields that should stay unchanged. Used by time-window tests where we
// want a deal whose window is in the future or past.
func (h *Harness) SetDealDates(t *testing.T, dealID, accountID string, start, end *time.Time) {
	t.Helper()
	h.WithTenant(t, accountID, func(tx *sql.Tx) {
		if start != nil {
			if _, err := tx.Exec("UPDATE deals SET start_date = $1, updated_at = now() WHERE id = $2", *start, dealID); err != nil {
				t.Fatalf("set deal start_date: %v", err)
			}
		}
		if end != nil {
			if _, err := tx.Exec("UPDATE deals SET end_date = $1, updated_at = now() WHERE id = $2", *end, dealID); err != nil {
				t.Fatalf("set deal end_date: %v", err)
			}
		}
	})
}

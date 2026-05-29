package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// ReconciliationResult holds the outcome of a daily reconciliation check.
type ReconciliationResult struct {
	Date               string
	CampaignID         string
	ExchangePublished  int64 // count from exchange logs
	ReportingConsumed  int64 // count from analytics store
	BillingRecorded    int64 // count from ledger
	Matched            bool
	MissingTraceIDs    []string
}

// Reconciler verifies that billing and reporting counts match.
type Reconciler struct {
	store  analytics.Store
	ledger *Ledger
}

// NewReconciler creates a reconciler.
func NewReconciler(store analytics.Store, ledger *Ledger) *Reconciler {
	return &Reconciler{store: store, ledger: ledger}
}

// Verify checks that impression counts in the analytics store match
// the ledger entry counts for a given campaign and day.
func (r *Reconciler) Verify(ctx context.Context, campaignID string, date time.Time) (*ReconciliationResult, error) {
	dayStart := date.Truncate(24 * time.Hour)
	dayEnd := dayStart.Add(24 * time.Hour)

	// Count impressions in analytics store
	qr, err := r.store.Query(ctx, analytics.QueryParams{
		Table:    "impressions",
		Metrics:  []string{"count"},
		Filters:  map[string]string{"campaign_id": campaignID},
		TimeFrom: dayStart,
		TimeTo:   dayEnd,
	})
	if err != nil {
		return nil, fmt.Errorf("query analytics: %w", err)
	}

	var reportingCount int64
	if len(qr.Rows) > 0 {
		if v, ok := qr.Rows[0][0].(int64); ok {
			reportingCount = v
		}
	}

	// Count billing entries in ledger
	var billingCount int64
	for _, e := range r.ledger.Entries() {
		if e.CampaignID == campaignID &&
			(e.Type == EntrySpend || e.Type == EntrySettlement) &&
			!e.Timestamp.Before(dayStart) && e.Timestamp.Before(dayEnd) {
			billingCount++
		}
	}

	matched := reportingCount == billingCount

	return &ReconciliationResult{
		Date:              dayStart.Format("2006-01-02"),
		CampaignID:        campaignID,
		ReportingConsumed: reportingCount,
		BillingRecorded:   billingCount,
		Matched:           matched,
	}, nil
}

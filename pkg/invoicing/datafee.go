// datafee.go — receivables for data-monetization fees (ADR 0009 phase 2).
//
// The impression-time accrual (cmd/reporting/datafee.go) debits
// extseat:{seat}:payable in the ledger, but external seats hold no prepay
// balance — the money is collected by invoicing the buyer out-of-band. This
// generator sums data_fee_earnings per winner_seat over a period into one
// data_fee_receivables row (with a per-owner/segment breakdown for the
// operator), idempotently: re-running a period replaces totals + breakdown
// via the UNIQUE (winner_seat, period) upsert, and never touches rows whose
// status has moved past draft.
package invoicing

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// receivableLine is one (owner, segment) slice of a seat's receivable.
type receivableLine struct {
	OwnerAccountID string `json:"owner_account_id"`
	SegmentID      string `json:"segment_id"`
	Impressions    int64  `json:"impressions"`
	FeeMicros      int64  `json:"fee_micros"`
}

// GenerateDataFeeReceivables writes one draft receivable per external seat
// with data-fee accruals in [periodStart, periodEnd). Returns the seats
// invoiced. Seats whose existing row for the period is no longer 'draft'
// (sent/paid/void) are left untouched — a late re-run must not rewrite an
// issued invoice.
func GenerateDataFeeReceivables(ctx context.Context, db DB, periodStart, periodEnd time.Time) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT winner_seat, account_id::text, segment_id, count(*), SUM(fee_micros)
FROM data_fee_earnings
WHERE created_at >= $1 AND created_at < $2
GROUP BY winner_seat, account_id, segment_id
ORDER BY winner_seat`, periodStart, periodEnd)
	if err != nil {
		return nil, fmt.Errorf("aggregate data fees: %w", err)
	}
	defer rows.Close()

	lines := map[string][]receivableLine{}
	for rows.Next() {
		var seat string
		var l receivableLine
		if err := rows.Scan(&seat, &l.OwnerAccountID, &l.SegmentID, &l.Impressions, &l.FeeMicros); err != nil {
			return nil, fmt.Errorf("scan data-fee aggregate: %w", err)
		}
		lines[seat] = append(lines[seat], l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	seats := make([]string, 0, len(lines))
	for seat, ls := range lines {
		var imps, total int64
		for _, l := range ls {
			imps += l.Impressions
			total += l.FeeMicros
		}
		breakdown, _ := json.Marshal(ls)
		// The status guard lives in the ON CONFLICT predicate: an issued
		// (non-draft) receivable for the period is never rewritten.
		if _, err := tx.ExecContext(ctx, `
INSERT INTO data_fee_receivables
  (winner_seat, period_start, period_end, impressions, total_micros, breakdown)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (winner_seat, period_start, period_end) DO UPDATE
  SET impressions = EXCLUDED.impressions,
      total_micros = EXCLUDED.total_micros,
      breakdown = EXCLUDED.breakdown,
      updated_at = now()
  WHERE data_fee_receivables.status = 'draft'`,
			seat, periodStart, periodEnd, imps, total, breakdown); err != nil {
			return nil, fmt.Errorf("upsert receivable for %s: %w", seat, err)
		}
		seats = append(seats, seat)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return seats, nil
}

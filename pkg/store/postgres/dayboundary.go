package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// FlightTransition is one line-item status change produced by the day-boundary
// job's IO-flight processing — enough for the caller to publish a
// CampaignStateEvent + cache invalidate so the DSP reloads and starts/stops
// bidding accordingly.
type FlightTransition struct {
	LineItemID string
	AccountID  string
	OldStatus  string
	NewStatus  string
}

// ActivateFlights moves insertion orders whose flight window now includes `day`
// from draft to active, and cascades their approved line items to live. Flight
// dates live on the IO (line_items have none); the bid path gates on
// line_item.status, so activation must cascade or nothing starts bidding.
// Returns the line-item transitions.
//
// Platform-wide job: relies on the connecting role bypassing RLS (dev superuser
// locally; a service role in prod), so it doesn't SET LOCAL a tenant.
func (s *Store) ActivateFlights(ctx context.Context, day time.Time) ([]FlightTransition, error) {
	return s.transitionIOFlights(ctx,
		`status = 'draft' AND start_date <= $1 AND end_date >= $1`, day, "active",
		[]string{"approved"}, "live")
}

// EndFlights moves active insertion orders whose end_date has passed to ended,
// and cascades their live/paused line items to ended (there's no FK cascade, so
// this is explicit — otherwise bids continue after the flight ends).
func (s *Store) EndFlights(ctx context.Context, day time.Time) ([]FlightTransition, error) {
	return s.transitionIOFlights(ctx,
		`status = 'active' AND end_date < $1`, day, "ended",
		[]string{"live", "paused"}, "ended")
}

func (s *Store) transitionIOFlights(ctx context.Context, ioPredicate string, day time.Time, ioNewStatus string, liFrom []string, liNew string) ([]FlightTransition, error) {
	tx, err := s.primary.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin flight tx: %w", err)
	}
	defer tx.Rollback()

	// Which IOs transition. ioPredicate binds `day` as $1.
	ioRows, err := tx.QueryContext(ctx, `SELECT id::text FROM insertion_orders WHERE `+ioPredicate, day)
	if err != nil {
		return nil, fmt.Errorf("select target IOs: %w", err)
	}
	var ioIDs []string
	for ioRows.Next() {
		var id string
		if err := ioRows.Scan(&id); err != nil {
			ioRows.Close()
			return nil, err
		}
		ioIDs = append(ioIDs, id)
	}
	ioRows.Close()
	if err := ioRows.Err(); err != nil {
		return nil, err
	}
	if len(ioIDs) == 0 {
		return nil, tx.Commit()
	}

	// Capture the cascading line items (with their OLD status) before updating,
	// so the caller can publish accurate old→new state-change events.
	liRows, err := tx.QueryContext(ctx,
		`SELECT id::text, account_id::text, status FROM line_items
		 WHERE insertion_order_id = ANY($1::uuid[]) AND status = ANY($2::text[])`,
		pq.StringArray(ioIDs), pq.StringArray(liFrom))
	if err != nil {
		return nil, fmt.Errorf("select cascade line items: %w", err)
	}
	var transitions []FlightTransition
	for liRows.Next() {
		var t FlightTransition
		if err := liRows.Scan(&t.LineItemID, &t.AccountID, &t.OldStatus); err != nil {
			liRows.Close()
			return nil, err
		}
		t.NewStatus = liNew
		transitions = append(transitions, t)
	}
	liRows.Close()
	if err := liRows.Err(); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE insertion_orders SET status = $1, updated_at = now() WHERE id = ANY($2::uuid[])`,
		ioNewStatus, pq.StringArray(ioIDs)); err != nil {
		return nil, fmt.Errorf("update IOs: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE line_items SET status = $1, updated_at = now()
		 WHERE insertion_order_id = ANY($2::uuid[]) AND status = ANY($3::text[])`,
		liNew, pq.StringArray(ioIDs), pq.StringArray(liFrom)); err != nil {
		return nil, fmt.Errorf("cascade line items: %w", err)
	}
	return transitions, tx.Commit()
}

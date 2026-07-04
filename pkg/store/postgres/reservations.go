package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
)

// ReservationContextStore persists reserve/settle auction context so the
// billing engine can settle on a ledger backend that doesn't retain string
// metadata (TigerBeetle). Billing-internal: keyed by trace_id, no tenant
// scoping. Implements billing.ReservationStore.
type ReservationContextStore struct {
	Store *Store
}

// SaveReservation upserts the context by trace_id (NATS redelivers reserves).
func (s *ReservationContextStore) SaveReservation(ctx context.Context, rc billing.ReservationContext) error {
	if s.Store == nil {
		return sql.ErrConnDone
	}
	_, err := s.Store.primary.ExecContext(ctx, `
INSERT INTO reservation_context
    (trace_id, campaign_id, creative_id, placement_id, publisher_id, advertiser_id, deal_type, currency, amount, bid_model)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (trace_id) DO UPDATE SET
    campaign_id = EXCLUDED.campaign_id, creative_id = EXCLUDED.creative_id,
    placement_id = EXCLUDED.placement_id, publisher_id = EXCLUDED.publisher_id,
    advertiser_id = EXCLUDED.advertiser_id, deal_type = EXCLUDED.deal_type,
    currency = EXCLUDED.currency, amount = EXCLUDED.amount, bid_model = EXCLUDED.bid_model`,
		rc.TraceID, rc.CampaignID, rc.CreativeID, rc.PlacementID, rc.PublisherID,
		rc.AdvertiserID, rc.DealType, nz(rc.Currency, "USD"), rc.Amount, rc.BidModel)
	if err != nil {
		return fmt.Errorf("save reservation context: %w", err)
	}
	return nil
}

// GetReservation recovers the context for a trace_id. (false, nil) when none.
func (s *ReservationContextStore) GetReservation(ctx context.Context, traceID string) (billing.ReservationContext, bool, error) {
	var rc billing.ReservationContext
	if s.Store == nil {
		return rc, false, sql.ErrConnDone
	}
	err := s.Store.read.QueryRowContext(ctx, `
SELECT trace_id, COALESCE(campaign_id,''), COALESCE(creative_id,''), COALESCE(placement_id,''),
       COALESCE(publisher_id,''), COALESCE(advertiser_id,''), COALESCE(deal_type,''),
       currency, amount::float8, bid_model
FROM reservation_context WHERE trace_id = $1`, traceID).
		Scan(&rc.TraceID, &rc.CampaignID, &rc.CreativeID, &rc.PlacementID,
			&rc.PublisherID, &rc.AdvertiserID, &rc.DealType, &rc.Currency, &rc.Amount, &rc.BidModel)
	if err == sql.ErrNoRows {
		return rc, false, nil
	}
	if err != nil {
		return rc, false, fmt.Errorf("get reservation context: %w", err)
	}
	return rc, true, nil
}

func nz(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

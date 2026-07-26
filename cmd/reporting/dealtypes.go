// Deal-id → deal-type resolver for billing's contract fee modifiers.
//
// The impression beacon carries the winning deal's ID (deal=<uuid>), but
// Contract.DealTypeModifiers keys by deal TYPE (pg/pmp/preferred/open) —
// and the spend construction used to pass the raw ID straight into
// SpendEvent.DealType, so a modifier could never match: deal-won
// impressions always billed at the open-market fee. This resolver closes
// the gap with a cached Postgres lookup; deal types are immutable in
// practice, so entries cache for the process lifetime.
//
// Fail-open by design: an unknown/empty/unresolvable deal id yields "" —
// billing then applies no modifier (open-market fee). That's a fee-shaping
// miss, not money corruption, so it must never block billing.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

type pgDealTypeSource struct {
	dbURL string
	log   *slog.Logger

	mu    sync.Mutex
	db    *sql.DB
	cache map[string]string // deal id -> deal_type ("" = known-miss)
}

func newPGDealTypeSource(dbURL string, log *slog.Logger) *pgDealTypeSource {
	return &pgDealTypeSource{dbURL: dbURL, log: log, cache: map[string]string{}}
}

// For resolves a deal id to its deal_type; "" when unknown.
func (s *pgDealTypeSource) For(ctx context.Context, dealID string) string {
	if dealID == "" {
		return ""
	}
	if _, err := uuid.Parse(dealID); err != nil {
		return "" // external/synthetic ids aren't our deals table's PK
	}

	s.mu.Lock()
	if t, hit := s.cache[dealID]; hit {
		s.mu.Unlock()
		return t
	}
	db := s.db
	s.mu.Unlock()

	if db == nil {
		if s.dbURL == "" {
			return ""
		}
		opened, err := sql.Open("postgres", s.dbURL)
		if err != nil {
			s.log.Error("deal type source: postgres open failed", "error", err)
			return ""
		}
		opened.SetMaxOpenConns(2)
		opened.SetMaxIdleConns(1)
		opened.SetConnMaxLifetime(5 * time.Minute)
		s.mu.Lock()
		if s.db == nil {
			s.db = opened
		} else {
			_ = opened.Close()
		}
		db = s.db
		s.mu.Unlock()
	}

	var dealType string
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := db.QueryRowContext(qctx, `SELECT deal_type FROM deals WHERE id = $1`, dealID).Scan(&dealType)
	if err != nil && err != sql.ErrNoRows {
		// Transient failure: fail open for THIS event, don't cache the miss.
		s.log.Error("deal type source: lookup failed", "deal_id", dealID, "error", err)
		return ""
	}

	s.mu.Lock()
	s.cache[dealID] = dealType
	if len(s.cache) > 8192 {
		s.cache = map[string]string{dealID: dealType}
	}
	s.mu.Unlock()
	return dealType
}

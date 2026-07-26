// Postgres-backed exchange-rate source for the billing engine's USD
// normalization (billing.SetRateSource). The exchange_rates table shipped
// in migration 012 and sat consumer-less for months — non-USD spend events
// were implicitly treated as dollars. Now every non-USD SpendEvent is
// converted (or refused) before money moves.
//
// Rates change ~daily, so lookups cache per (currency, day) — the bid/event
// hot path never waits on Postgres after the first hit. Lazy-connect +
// fail-closed: an unreachable DB means "no rate", which the engine treats
// as unbillable (loud drop) rather than a silent 1:1 booking.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
)

type pgRateSource struct {
	dbURL string
	log   *slog.Logger

	mu    sync.Mutex
	db    *sql.DB
	cache map[string]rateEntry // "EUR|2026-07-26" -> rate
}

type rateEntry struct {
	rate float64
	ok   bool
}

func newPGRateSource(dbURL string, log *slog.Logger) *pgRateSource {
	return &pgRateSource{dbURL: dbURL, log: log, cache: map[string]rateEntry{}}
}

// Rate implements billing.RateSource.
func (s *pgRateSource) Rate(ctx context.Context, currency string, on time.Time) (float64, bool) {
	day := on.UTC().Format("2006-01-02")
	key := currency + "|" + day

	s.mu.Lock()
	if e, hit := s.cache[key]; hit {
		s.mu.Unlock()
		return e.rate, e.ok
	}
	db := s.db
	s.mu.Unlock()

	if db == nil {
		if s.dbURL == "" {
			return 0, false
		}
		opened, err := sql.Open("postgres", s.dbURL)
		if err != nil {
			s.log.Error("rate source: postgres open failed", "error", err)
			return 0, false
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

	// Most recent rate effective on or before the event date.
	var rate float64
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := db.QueryRowContext(qctx, `
SELECT rate FROM exchange_rates
WHERE base_currency = 'USD' AND target_currency = $1 AND effective_date <= $2::date
ORDER BY effective_date DESC LIMIT 1`, currency, day).Scan(&rate)
	found := err == nil && rate > 0
	if err != nil && err != sql.ErrNoRows {
		// Transient DB failure: fail closed for THIS event but don't cache
		// the miss — the next event retries.
		s.log.Error("rate source: lookup failed", "currency", currency, "error", err)
		return 0, false
	}

	s.mu.Lock()
	s.cache[key] = rateEntry{rate: rate, ok: found}
	// Day-keyed entries accumulate one per currency per day — bound it.
	if len(s.cache) > 4096 {
		s.cache = map[string]rateEntry{key: {rate: rate, ok: found}}
	}
	s.mu.Unlock()
	return rate, found
}

var _ billing.RateSource = (*pgRateSource)(nil).Rate

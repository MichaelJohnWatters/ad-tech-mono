// Per-line-item attribution overrides (gap G5).
//
// The global attribution.* config keys are the platform defaults; a campaign may
// override the windows / require-viewability on its targeting_rules row. This
// resolver reads that JSON with a lazy, TTL-cached Postgres lookup (mirrors
// dealtypes.go) — attribution is low-volume, so a per-conversion query behind a
// short cache is fine, and the TTL lets a portal edit take effect without a
// restart.
//
// Fail-open by design: any error / no row / empty '{}' yields nil, and the
// attributor falls back to the global keys. An override is a measurement knob,
// never money, so it must never block attribution.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/google/uuid"
)

// attributionOverride is the per-campaign override JSON. All fields optional;
// a nil field means "use the global default".
type attributionOverride struct {
	ViewWindowHours  *int    `json:"view_window_hours,omitempty"`
	ClickWindowHours *int    `json:"click_window_hours,omitempty"`
	RequireViewable  *bool   `json:"require_viewable,omitempty"`
	Model            *string `json:"model,omitempty"`
}

type cachedOverride struct {
	val *attributionOverride // nil = known "no override"
	at  time.Time
}

type pgAttributionConfigSource struct {
	dbURL string
	log   *slog.Logger
	ttl   time.Duration

	mu    sync.Mutex
	db    *sql.DB
	cache map[string]cachedOverride
}

func newPGAttributionConfigSource(dbURL string, log *slog.Logger) *pgAttributionConfigSource {
	return &pgAttributionConfigSource{dbURL: dbURL, log: log, ttl: 60 * time.Second, cache: map[string]cachedOverride{}}
}

// Get returns the per-campaign override, or nil when there is none / on any
// error (caller falls back to the global defaults).
func (s *pgAttributionConfigSource) Get(ctx context.Context, campaignID string) *attributionOverride {
	if campaignID == "" {
		return nil
	}
	if _, err := uuid.Parse(campaignID); err != nil {
		return nil // campaign_id == line_item.id (a uuid); anything else can't match
	}

	s.mu.Lock()
	if c, hit := s.cache[campaignID]; hit && time.Since(c.at) < s.ttl {
		s.mu.Unlock()
		return c.val
	}
	db := s.db
	s.mu.Unlock()

	if db == nil {
		if s.dbURL == "" {
			return nil
		}
		opened, err := sql.Open("postgres", s.dbURL)
		if err != nil {
			s.log.Error("attribution config source: postgres open failed", "error", err)
			return nil
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

	var raw []byte
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// Attribution runs across all advertisers' campaigns (targeting_rules has
	// RLS), so read under the platform hatch — same as the deal-type resolver.
	err := postgres.NewFromDB(db).QueryRowPlatform(qctx, func(row *sql.Row) error {
		return row.Scan(&raw)
	}, `SELECT attribution_config FROM targeting_rules WHERE line_item_id = $1`, campaignID)
	if err != nil && err != sql.ErrNoRows {
		// Transient failure: fail open for THIS event, don't cache the miss.
		s.log.Error("attribution config source: lookup failed", "campaign_id", campaignID, "error", err)
		return nil
	}

	var ov *attributionOverride
	if err == nil && len(raw) > 0 {
		var parsed attributionOverride
		if e := json.Unmarshal(raw, &parsed); e != nil {
			s.log.Warn("attribution config source: bad json, using globals", "campaign_id", campaignID, "error", e)
		} else if parsed != (attributionOverride{}) {
			ov = &parsed
		}
	}

	s.mu.Lock()
	s.cache[campaignID] = cachedOverride{val: ov, at: time.Now()}
	if len(s.cache) > 8192 {
		s.cache = map[string]cachedOverride{campaignID: {val: ov, at: time.Now()}}
	}
	s.mu.Unlock()
	return ov
}

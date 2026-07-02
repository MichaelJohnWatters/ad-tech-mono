// cmd/dayboundary runs at the day boundary to handle flight-date transitions
// (and, later, daily budget resets + IO depletion). Designed as a K8s CronJob;
// idempotent — safe to re-run.
//
// Phase 1 (ADR 0005 §2): IO flight transitions with an explicit line-item
// cascade, driven off Postgres. Daily budget reset + IO budget depletion (which
// need Redis spend + a snapshot table) are later phases and are logged as
// pending for now.
//
// Usage:
//
//	go run ./cmd/dayboundary                    # today, UTC
//	go run ./cmd/dayboundary --date 2024-06-15  # a specific date
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// DayBoundaryResult tracks what the job did.
type DayBoundaryResult struct {
	Date             string
	CampaignsStarted int // line items cascaded approved → live
	CampaignsEnded   int // line items cascaded live/paused → ended
	IOsActivated     int
	IOsEnded         int
	ProcessedAt      time.Time
	Duration         time.Duration
}

func main() {
	dateStr := flag.String("date", "", "date to process (YYYY-MM-DD), defaults to today UTC")
	flag.Parse()

	log := logger.New("dayboundary")

	var processDate time.Time
	if *dateStr != "" {
		d, err := time.Parse("2006-01-02", *dateStr)
		if err != nil {
			log.Error("invalid date", "date", *dateStr, "error", err)
			os.Exit(1)
		}
		processDate = d
	} else {
		processDate = time.Now().UTC().Truncate(24 * time.Hour)
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Error("DATABASE_URL not set; day-boundary job cannot run")
		os.Exit(1)
	}
	store, err := postgres.New(postgres.Config{PrimaryURL: dbURL, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: 5 * time.Minute})
	if err != nil {
		log.Error("postgres connect failed", "error", err)
		os.Exit(1)
	}

	// Events are best-effort: without NATS the DB transitions still happen,
	// caches just pick them up on their next poll instead of immediately.
	var bus events.EventBus
	if natsURL := os.Getenv("NATS_URL"); natsURL != "" {
		if b, err := natsbus.New(natsURL, "dayboundary", log); err != nil {
			log.Warn("nats unavailable; transitions won't be broadcast (caches poll)", "error", err)
		} else {
			bus = b
			defer b.Close()
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	log.Info("day boundary job starting", "date", processDate.Format("2006-01-02"))
	result := runDayBoundary(ctx, store, bus, log, processDate)
	log.Info("day boundary job complete",
		"date", result.Date,
		"ios_activated", result.IOsActivated,
		"campaigns_started", result.CampaignsStarted,
		"ios_ended", result.IOsEnded,
		"campaigns_ended", result.CampaignsEnded,
		"duration_ms", result.Duration.Milliseconds(),
	)
}

func runDayBoundary(ctx context.Context, store *postgres.Store, bus events.EventBus, log *slog.Logger, date time.Time) DayBoundaryResult {
	start := time.Now()
	result := DayBoundaryResult{Date: date.Format("2006-01-02"), ProcessedAt: time.Now().UTC()}
	var pub *events.Publisher
	if bus != nil {
		pub = events.NewPublisher(bus, log)
	}

	// Activate flights that started (IO draft → active, cascade approved → live).
	if activated, err := store.ActivateFlights(ctx, date); err != nil {
		log.Error("activate flights failed", "error", err)
	} else {
		result.CampaignsStarted = len(activated)
		publishTransitions(ctx, pub, log, activated, "flight_start")
	}

	// End flights that finished (IO active → ended, cascade live/paused → ended).
	if ended, err := store.EndFlights(ctx, date); err != nil {
		log.Error("end flights failed", "error", err)
	} else {
		result.CampaignsEnded = len(ended)
		publishTransitions(ctx, pub, log, ended, "flight_end")
	}

	// One cache-invalidate so the DSP warm cache reloads immediately rather than
	// waiting for its poll. Payload is advisory — the cache reloads wholesale.
	if bus != nil && (result.CampaignsStarted > 0 || result.CampaignsEnded > 0) {
		if err := bus.Publish(ctx, events.SubjectCacheInvalidateCampaigns, []byte(`{"source":"dayboundary"}`)); err != nil {
			log.Warn("dayboundary: campaign cache invalidate failed", "error", err)
		}
	}

	// Pending later phases (need Redis spend + a daily_spend_snapshots table):
	//   - daily budget reset + snapshot
	//   - IO budget depletion (spend >= budget)
	// See docs/adr/0005-deferred-followups.md §2 Phases 2–3.

	result.Duration = time.Since(start)
	return result
}

// publishTransitions emits a CampaignStateEvent per cascaded line item so
// reporting/ops see the pause/resume timeline. Best-effort.
func publishTransitions(ctx context.Context, pub *events.Publisher, log *slog.Logger, transitions []postgres.FlightTransition, reason string) {
	if pub == nil {
		return
	}
	for _, t := range transitions {
		if err := pub.CampaignStateChanged(ctx, events.CampaignStateEvent{
			CampaignID: t.LineItemID, AccountID: t.AccountID,
			OldState: t.OldStatus, NewState: t.NewStatus, Reason: reason, Timestamp: time.Now(),
		}); err != nil {
			log.Warn("dayboundary: publish state change failed", "line_item", t.LineItemID, "error", err)
		}
	}
}

// cmd/dayboundary runs at midnight UTC (or per-timezone) to handle
// daily budget resets, flight date transitions, and IO budget checks.
//
// Designed as a K8s CronJob. Idempotent - safe to run multiple times.
//
// Usage:
//
//	go run ./cmd/dayboundary                    # run for current UTC day
//	go run ./cmd/dayboundary --date 2024-06-15  # run for specific date
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

// LineItemState represents a line item's lifecycle state.
type LineItemState struct {
	ID          string
	IOId        string
	Status      string // approved, live, paused, ended
	StartDate   time.Time
	EndDate     time.Time
	DailyBudget float64
	TotalBudget float64
	TotalSpend  float64
	DailySpend  float64
	Timezone    string
}

// IOState represents an insertion order's state.
type IOState struct {
	ID          string
	Status      string
	EndDate     time.Time
	TotalBudget float64
	TotalSpend  float64
}

// DayBoundaryResult tracks what the job did.
type DayBoundaryResult struct {
	Date              string
	BudgetsReset      int
	CampaignsStarted  int
	CampaignsEnded    int
	IOsDepleted       int
	IOsEnded          int
	ProcessedAt       time.Time
	Duration          time.Duration
}

func main() {
	dateStr := flag.String("date", "", "date to process (YYYY-MM-DD), defaults to today UTC")
	flag.Parse()

	log := logger.New("dayboundary")

	var processDate time.Time
	if *dateStr != "" {
		var err error
		processDate, err = time.Parse("2006-01-02", *dateStr)
		if err != nil {
			log.Error("invalid date", "date", *dateStr, "error", err)
			return
		}
	} else {
		processDate = time.Now().UTC().Truncate(24 * time.Hour)
	}

	log.Info("day boundary job starting", "date", processDate.Format("2006-01-02"))

	result := runDayBoundary(context.Background(), log, processDate)

	log.Info("day boundary job complete",
		"date", result.Date,
		"budgets_reset", result.BudgetsReset,
		"campaigns_started", result.CampaignsStarted,
		"campaigns_ended", result.CampaignsEnded,
		"ios_depleted", result.IOsDepleted,
		"ios_ended", result.IOsEnded,
		"duration_ms", result.Duration.Milliseconds(),
	)
}

func runDayBoundary(_ context.Context, log *slog.Logger, date time.Time) DayBoundaryResult {
	start := time.Now()
	result := DayBoundaryResult{
		Date:        date.Format("2006-01-02"),
		ProcessedAt: time.Now().UTC(),
	}

	// In production, these come from Postgres queries.
	// For now, use demo data to prove the logic works.
	lineItems := demoLineItems(date)
	ios := demoIOs(date)

	// Step 1: Daily budget resets
	for _, li := range lineItems {
		if li.Status == "live" && li.DailyBudget > 0 {
			// Snapshot today's spend
			log.Info("budget reset",
				"line_item", li.ID,
				"daily_spend", li.DailySpend,
				"daily_budget", li.DailyBudget,
			)
			// In production: write daily_spend_snapshots, reset Redis counter
			result.BudgetsReset++
		}
	}

	// Step 2: Flight date transitions
	yesterday := date.AddDate(0, 0, -1)
	for _, li := range lineItems {
		// Start campaigns whose start_date is today
		if li.Status == "approved" && !li.StartDate.After(date) {
			log.Info("campaign started",
				"line_item", li.ID,
				"start_date", li.StartDate.Format("2006-01-02"),
			)
			// In production: update Postgres status, load into Redis, publish NATS
			result.CampaignsStarted++
		}

		// End campaigns whose end_date was yesterday
		if li.Status == "live" && !li.EndDate.IsZero() && li.EndDate.Before(date) && !li.EndDate.Before(yesterday) {
			log.Info("campaign ended",
				"line_item", li.ID,
				"end_date", li.EndDate.Format("2006-01-02"),
			)
			result.CampaignsEnded++
		}
	}

	// Step 3: IO budget and flight checks
	for _, io := range ios {
		if io.Status == "active" && io.TotalSpend >= io.TotalBudget {
			log.Info("IO budget depleted",
				"io", io.ID,
				"spend", io.TotalSpend,
				"budget", io.TotalBudget,
			)
			result.IOsDepleted++
		}

		if io.Status == "active" && !io.EndDate.IsZero() && io.EndDate.Before(date) {
			log.Info("IO ended",
				"io", io.ID,
				"end_date", io.EndDate.Format("2006-01-02"),
			)
			result.IOsEnded++
		}
	}

	result.Duration = time.Since(start)
	return result
}

// demoLineItems returns demo line items for testing the day boundary job.
func demoLineItems(today time.Time) []LineItemState {
	return []LineItemState{
		{
			ID: "li-001", IOId: "io-001", Status: "live",
			StartDate: today.AddDate(0, 0, -7), EndDate: today.AddDate(0, 0, 23),
			DailyBudget: 500, TotalBudget: 10000, TotalSpend: 2100, DailySpend: 350,
		},
		{
			ID: "li-002", IOId: "io-001", Status: "live",
			StartDate: today.AddDate(0, 0, -3), EndDate: today.AddDate(0, 0, 27),
			DailyBudget: 1000, TotalBudget: 25000, TotalSpend: 2500, DailySpend: 850,
		},
		{
			ID: "li-new", IOId: "io-002", Status: "approved",
			StartDate: today, EndDate: today.AddDate(0, 1, 0),
			DailyBudget: 300, TotalBudget: 5000, TotalSpend: 0, DailySpend: 0,
		},
		{
			ID: "li-ending", IOId: "io-003", Status: "live",
			StartDate: today.AddDate(0, -1, 0), EndDate: today.AddDate(0, 0, -1),
			DailyBudget: 200, TotalBudget: 3000, TotalSpend: 2800, DailySpend: 180,
		},
	}
}

func demoIOs(today time.Time) []IOState {
	return []IOState{
		{ID: "io-001", Status: "active", EndDate: today.AddDate(0, 1, 0), TotalBudget: 35000, TotalSpend: 4600},
		{ID: "io-002", Status: "active", EndDate: today.AddDate(0, 1, 0), TotalBudget: 5000, TotalSpend: 0},
		{ID: "io-003", Status: "active", EndDate: today.AddDate(0, 0, -1), TotalBudget: 3000, TotalSpend: 2800},
		{ID: "io-depleted", Status: "active", EndDate: today.AddDate(0, 1, 0), TotalBudget: 1000, TotalSpend: 1000},
	}
}

func init() {
	// Suppress unused import warning for fmt
	_ = fmt.Sprintf
}

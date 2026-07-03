package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/redis/go-redis/v9"
)

// resetAndReseedHandler is a dev-only convenience that wipes all tenant
// data and re-runs the seed profile. Used by the publisher simulator's
// "Reset & reseed" button so you don't need to drop to a terminal and
// `tilt trigger migrate-reset` between experiments.
//
// What it does, in order:
//  1. TRUNCATE every tenant table (RESTART IDENTITY CASCADE) — same table
//     list as the e2e harness Reset for consistency.
//  2. FLUSHDB on Redis — clears budget counters, freq caps, dedup keys,
//     the audience preloader's snapshot. Services lose their L2 cache
//     but they'll repopulate on the next preload / write.
//  3. Re-run `cmd/seed --profile standard` via exec. Spawning the binary
//     is cleaner than importing seed-internal packages here (the gateway
//     would otherwise pull in every Postgres insert helper).
//  4. Publish NATS cache invalidates on every warm-cache subject so
//     services drop their stale in-process caches immediately.
//
// Gated by debug.endpoints_enabled. Returns JSON with per-step timing
// + counts so the UI can show a useful summary.
//
// Not transactional — if step 3 fails mid-seed you can end up with a
// partial DB. Acceptable for a dev tool; just click the button again.
func resetAndReseedHandler(dbURL string, redisAddr string, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	// Tables in same order as tests/e2e/harness/reset.go. TRUNCATE ...
	// CASCADE handles the FK graph, but listing keeps intent explicit
	// and breaks loudly when a new migration adds a tenant table that
	// needs to be wiped here too.
	tables := []string{
		"line_item_creatives",
		"targeting_rules",
		"line_items",
		"creatives",
		"insertion_orders",
		"placements",
		"deals",
		"publishers",
		"audience_segment_members",
		"audience_segments",
		"budget_reservations",
		"ledger_entries",
		"invoices",
		"payouts",
		"adjustments",
		"advertiser_balances",
		"topups",
		"api_keys",
		"team_members",
		"accounts",
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if dbURL == "" {
			http.Error(w, "database.url not set; cannot reset", http.StatusServiceUnavailable)
			return
		}

		type stepResult struct {
			Step       string `json:"step"`
			OK         bool   `json:"ok"`
			DurationMs int64  `json:"duration_ms"`
			Detail     string `json:"detail,omitempty"`
		}
		var steps []stepResult
		addStep := func(name string, start time.Time, err error, detail string) {
			s := stepResult{Step: name, OK: err == nil, DurationMs: time.Since(start).Milliseconds(), Detail: detail}
			if err != nil {
				s.Detail = err.Error()
			}
			steps = append(steps, s)
		}

		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()

		// Step 1: TRUNCATE
		t1 := time.Now()
		db, err := sql.Open("postgres", dbURL)
		if err != nil {
			addStep("open postgres", t1, err, "")
			writeJSON(w, steps, http.StatusInternalServerError)
			return
		}
		defer db.Close()
		stmt := "TRUNCATE TABLE " + strings.Join(tables, ", ") + " RESTART IDENTITY CASCADE"
		_, err = db.ExecContext(ctx, stmt)
		addStep("truncate", t1, err, fmt.Sprintf("%d tables", len(tables)))
		if err != nil {
			log.Error("reset: truncate failed", "error", err)
			writeJSON(w, steps, http.StatusInternalServerError)
			return
		}

		// Step 2: FLUSHDB on Redis. Best-effort — a Redis blip shouldn't
		// block the whole reset since services treat Redis as fail-open.
		t2 := time.Now()
		rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
		err = rdb.FlushDB(ctx).Err()
		_ = rdb.Close()
		addStep("redis flushdb", t2, err, "")
		// Don't bail on Redis failure — continue to reseed.

		// Step 3: Re-run cmd/seed. Spawning the binary avoids dragging
		// every insert helper into the gateway binary's import graph.
		//
		// In pod mode the seed binary is at /seed (baked into the gateway
		// image alongside /profiles by build/Dockerfile.dev). Falls back to
		// `go run ./cmd/seed` for host-mode dev where the Go toolchain
		// is available and there's no pre-built binary.
		t3 := time.Now()
		seedCtx, seedCancel := context.WithTimeout(ctx, 45*time.Second)
		defer seedCancel()
		var cmd *exec.Cmd
		if _, err := os.Stat("/seed"); err == nil {
			cmd = exec.CommandContext(seedCtx, "/seed", "--profile", "standard",
				"--dsps-dir", "/profiles/dsps",
				"--publishers-dir", "/profiles/publishers",
				"--deals-dir", "/profiles/deals",
				"--direct-sold-dir", "/profiles/direct-sold")
		} else {
			cmd = exec.CommandContext(seedCtx, "go", "run", "./cmd/seed", "--profile", "standard")
		}
		out, err := cmd.CombinedOutput()
		detail := strings.TrimSpace(string(out))
		// Trim very long output; the JSON response shouldn't bloat with
		// thousands of lines if seed gets verbose.
		if len(detail) > 600 {
			detail = "…" + detail[len(detail)-600:]
		}
		addStep("reseed", t3, err, detail)
		if err != nil {
			log.Error("reset: reseed failed", "error", err, "output", string(out))
			writeJSON(w, steps, http.StatusInternalServerError)
			return
		}

		// Step 4: Publish cache invalidates so every warm cache in the
		// fleet drops its now-stale snapshot and reloads from the freshly
		// seeded Postgres. Without these the SSP/DSP/etc would happily
		// keep serving the pre-reset state until their poll interval
		// (default 30s) ticks.
		t4 := time.Now()
		var invalidateErrs []string
		subjects := []string{
			events.SubjectCacheInvalidateCampaigns,
			events.SubjectCacheInvalidatePlacements,
			events.SubjectCacheInvalidateCreatives,
			events.SubjectCacheInvalidatePublishers,
			events.SubjectCacheInvalidateDeals,
		}
		if bus != nil {
			payload, _ := json.Marshal(map[string]string{"source": "gateway-reset", "op": "reset"})
			for _, subj := range subjects {
				if err := bus.Publish(ctx, subj, payload); err != nil {
					invalidateErrs = append(invalidateErrs, subj+": "+err.Error())
				}
			}
		} else {
			invalidateErrs = append(invalidateErrs, "no bus configured")
		}
		var invErr error
		if len(invalidateErrs) > 0 {
			invErr = fmt.Errorf("%s", strings.Join(invalidateErrs, "; "))
		}
		addStep("publish invalidates", t4, invErr, fmt.Sprintf("%d subjects", len(subjects)))

		log.Info("reset+reseed complete", "total_ms", time.Since(t1).Milliseconds())
		writeJSON(w, steps, http.StatusOK)
	}
}

func writeJSON(w http.ResponseWriter, body any, status int) {
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

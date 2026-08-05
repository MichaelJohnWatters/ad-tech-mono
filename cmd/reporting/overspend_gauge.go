package main

// Overspend observability: the empirical check on the DSP balance gate. The
// gate is deliberately approximate (30s snapshot + ~1s-cached Redis delta),
// with a stated tolerance of "overspend bounded to seconds of win volume,
// billing reconciles". These gauges make that claim measurable instead of
// arithmetic: billing draws advertiser_balances down as spend settles, so a
// NEGATIVE balance IS overspend — the gate admitted more spend than funding.
//
// Zero is the expected steady state (2026-08-05: $0.00 across ~500k
// impressions of soak + chaos). A sustained non-zero here means the gate's
// staleness budget no longer matches traffic — tighten the delta TTL or the
// snapshot cadence before touching anything else.

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// startOverspendGauge polls advertiser_balances every 30s and exports the
// negative-balance aggregate. No-op without a DB URL.
func startOverspendGauge(ctx context.Context, dbURL string, reg *prometheus.Registry, log *slog.Logger) {
	if dbURL == "" || reg == nil {
		return
	}
	accounts := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "adtech", Subsystem: "billing", Name: "overspent_accounts",
		Help: "Accounts whose settled spend exceeds their funding (balance < 0). Expected 0; sustained non-zero = the balance gate's staleness budget no longer matches traffic.",
	})
	totalUSD := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "adtech", Subsystem: "billing", Name: "overspend_usd_total",
		Help: "Sum of negative advertiser balances, in USD (positive number = money overspent).",
	})
	reg.MustRegister(accounts, totalUSD)

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("overspend gauge: postgres open failed", "error", err)
		return
	}
	db.SetMaxOpenConns(1)

	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			var n int
			var usd float64
			qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := db.QueryRowContext(qctx,
				`SELECT count(*), coalesce(-sum(balance), 0)
				 FROM advertiser_balances WHERE balance < 0`).Scan(&n, &usd)
			cancel()
			if err != nil {
				log.Warn("overspend gauge: query failed", "error", err)
			} else {
				accounts.Set(float64(n))
				totalUSD.Set(usd)
				if n > 0 {
					log.Error("advertiser overspend detected — balance gate staleness exceeds traffic tolerance",
						"accounts", n, "total_usd", usd)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	log.Info("overspend gauge running (30s poll on advertiser_balances)")
}

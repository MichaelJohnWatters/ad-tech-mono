package main

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	tbledger "github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing/tigerbeetle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tb"
)

// selectLedger returns the billing.Ledger implementation chosen by
// billing.ledger_backend ("memory" or "tigerbeetle").
//
// Memory is the default — dev/CI environments boot without TigerBeetle.
// For tigerbeetle, an unreachable cluster is treated as a fatal config
// problem (os.Exit) rather than a silent fail-open: the ledger is the
// source of truth for cost, and a memory fallback would silently lose
// every spend across restarts. The operator either fixes TB or flips
// the backend back to memory explicitly.
func selectLedger(cfg *config.Config, log *slog.Logger, lc *lifecycle.Lifecycle) billing.Ledger {
	backend := strings.ToLower(strings.TrimSpace(cfg.Get("billing.ledger_backend", "memory")))
	switch backend {
	case "memory", "":
		log.Info("billing ledger: memory backend")
		return billing.NewMemoryLedger()
	case "tigerbeetle":
		addrs := splitAndTrim(cfg.Get("billing.tigerbeetle_addresses", "127.0.0.1:3033"))
		if len(addrs) == 0 {
			log.Error("billing ledger: tigerbeetle backend requested but billing.tigerbeetle_addresses is empty")
			os.Exit(1)
		}
		client, err := tb.NewClient(addrs)
		if err != nil {
			log.Error("billing ledger: failed to connect to tigerbeetle",
				"addresses", addrs, "error", err)
			os.Exit(1)
		}
		lc.OnShutdown("tigerbeetle-client", func(_ context.Context) error {
			client.Close()
			return nil
		})
		log.Info("billing ledger: tigerbeetle backend", "addresses", addrs)
		return tbledger.New(client, log)
	default:
		log.Error("billing ledger: unknown backend, refusing to boot",
			"billing.ledger_backend", backend, "valid", []string{"memory", "tigerbeetle"})
		os.Exit(1)
		return nil
	}
}

func splitAndTrim(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

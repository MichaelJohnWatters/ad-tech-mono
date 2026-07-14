package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	tbledger "github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing/tigerbeetle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
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
	backend := strings.ToLower(strings.TrimSpace(keys.Billing.LedgerBackend.Get(cfg)))
	switch backend {
	case "memory", "":
		log.Info("billing ledger: memory backend")
		return billing.NewMemoryLedger()
	case "tigerbeetle":
		addrs := splitAndTrim(keys.Billing.TigerBeetleAddresses.Get(cfg))
		if len(addrs) == 0 {
			log.Error("billing ledger: tigerbeetle backend requested but billing.tigerbeetle_addresses is empty")
			os.Exit(1)
		}
		// The tigerbeetle client only accepts IP:port — a k8s service name
		// ("tigerbeetle:3000") is rejected as "Invalid client cluster
		// address". Resolve hostnames here so the same config works for the
		// in-cluster pod (DNS service name) and a local process (127.0.0.1).
		addrs, err := resolveAddrs(addrs)
		if err != nil {
			log.Error("billing ledger: failed to resolve tigerbeetle address",
				"addresses", addrs, "error", err)
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

// resolveAddrs rewrites host:port addresses whose host is a DNS name into
// IP:port (first A record). Bare ports and IP literals pass through.
func resolveAddrs(addrs []string) ([]string, error) {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		host, port, err := net.SplitHostPort(a)
		if err != nil || host == "" || net.ParseIP(host) != nil {
			// Bare port ("3000"), IP literal, or unparseable — pass through
			// and let the client report it.
			out = append(out, a)
			continue
		}
		ips, err := net.LookupHost(host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("resolve %s: %w", host, err)
		}
		out = append(out, net.JoinHostPort(ips[0], port))
	}
	return out, nil
}

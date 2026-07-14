package main

import (
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
)

// The exchange's owned config keys are declared once as typed handles in
// pkg/config/keys/exchange.go; main.go passes keys.ExchangeSchema() to
// config.Setup so the pod publishes the full schema (those entries +
// platform defaults) into its service_registry row. Adding a new
// auction-side knob = add a handle there.

// Knobs is the exchange's typed config accessor. See cmd/dsp/config.go for
// the pattern explanation.
type Knobs struct {
	cfg *config.Config

	// Live (consumed per-request in the auction handler — atomic swap so
	// UI edits land without a restart).
	BidTimeout *config.LiveDuration
}

// NewKnobs binds the live keys to the manager. Called once at boot.
func NewKnobs(sc *config.ServiceConfig) *Knobs {
	return &Knobs{
		cfg:        sc.Cfg,
		BidTimeout: config.NewLiveDuration(sc.Manager, sc.Cfg, keys.Exchange.BidTimeout.Key(), 500*time.Millisecond),
	}
}

// Channel is the inventory-channel restriction for this exchange instance
// (all, display, video, ctv, native, audio). TierLive — read per auction.
func (k *Knobs) Channel() string { return keys.Exchange.Channel.Get(k.cfg) }

// WinLossEnabled controls whether the exchange sends win/loss callbacks to
// DSPs. TierLive — read per auction.
func (k *Knobs) WinLossEnabled() bool { return keys.Exchange.WinLossEnabled.Get(k.cfg) }

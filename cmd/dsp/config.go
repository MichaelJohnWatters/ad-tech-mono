package main

import (
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
)

// The DSP's owned config keys are declared once as typed handles in
// pkg/config/keys/dsp.go; main.go passes keys.DSPSchema() to config.Setup
// so the pod publishes the full schema (those entries + the platform-shared
// defaults) into its service_registry row. Adding a new DSP knob = add a
// handle there.

// Knobs is the DSP service's typed config accessor. Methods read live every
// call (scalars are cheap RLock + map lookup); Live* fields are pre-bound
// atomic.Pointer holders for keys whose value is consumed inside a
// constructed object (e.g. BudgetTracker holds a TTL — has to be a Live*
// so a UI edit actually takes effect on the next budget write).
type Knobs struct {
	cfg *config.Config

	// Live (consumed inside a constructor; needs atomic swap on change)
	BudgetResetInterval *config.LiveDuration
}

// NewKnobs binds the live keys to the manager and returns the typed
// accessor. Called once at boot.
func NewKnobs(sc *config.ServiceConfig) *Knobs {
	return &Knobs{
		cfg:                 sc.Cfg,
		BudgetResetInterval: config.NewLiveDuration(sc.Manager, sc.Cfg, keys.DSP.BudgetResetInterval.Key(), 24*time.Hour),
	}
}

// Profile returns the DSP profile name (internal, competitor1, competitor2).
// TierStatic — env var override at boot wins; UI edits don't apply.
func (k *Knobs) Profile() string { return keys.DSP.Profile.Get(k.cfg) }

// DailyBudgetDefault is the daily budget applied when a campaign doesn't
// set its own. TierLive — read on every campaign creation path.
func (k *Knobs) DailyBudgetDefault() float64 {
	return keys.DSP.DailyBudgetDefault.Get(k.cfg)
}

// MaxBidModifier caps how much a targeting rule can multiply a base bid.
// TierLive — read at every bid evaluation.
func (k *Knobs) MaxBidModifier() float64 { return keys.DSP.MaxBidModifier.Get(k.cfg) }

// NoisePct is the ±% jitter added to every bid. TierLive.
func (k *Knobs) NoisePct() float64 { return keys.DSP.NoisePct.Get(k.cfg) }

// NoBidRate is the probability (0-1) of a random no-bid. TierLive.
func (k *Knobs) NoBidRate() float64 { return keys.DSP.NoBidRate.Get(k.cfg) }

// FlightPacingEnabled: pace lifetime budget across the IO flight window instead
// of daily budget across a 24h day (for campaigns that have both). TierLive.
func (k *Knobs) FlightPacingEnabled() bool { return keys.DSP.FlightPacingEnabled.Get(k.cfg) }

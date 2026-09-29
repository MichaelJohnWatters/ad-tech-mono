// Package pacing controls how fast a campaign spends its budget.
//
// Without pacing, a campaign burns through its daily budget by 10am
// and misses afternoon inventory. The pacer runs on every bid request
// and decides: should we bid or skip this request to save budget?
//
// Modes: even (smooth), ASAP (fast), front-loaded (80% by noon).
//
// Usage:
//
//	pacer := pacing.New(clk, pacing.Config{Mode: "even", DailyBudget: 100})
//	if pacer.ShouldBid(actualSpend) { /* proceed to bid */ }
package pacing

import (
	"math/rand"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

// Mode determines how spend is distributed throughout the day.
type Mode string

const (
	ModeEven        Mode = "even"         // smooth spend across the day
	ModeASAP        Mode = "asap"         // spend as fast as possible
	ModeFrontLoaded Mode = "front_loaded" // 80% by noon, 20% afternoon
)

// Config holds pacing configuration for a line item.
type Config struct {
	Mode        Mode
	DailyBudget float64
	DayStartUTC time.Time // midnight UTC (or timezone-adjusted)
	Timezone    string    // for timezone-aware day boundaries

	// Flight-aware pacing (optional). When LifetimeBudget > 0 AND FlightEnd is
	// after FlightStart, the pace curve is spread over the flight window
	// [FlightStart, FlightEnd] against LifetimeBudget — instead of over a fixed
	// 24h day against DailyBudget. This is how real pacers work at flight
	// granularity, and it's what lets a SHORT-flight campaign pace visibly over
	// minutes rather than a hardcoded day. Zero values → legacy daily pacing
	// (behaviour unchanged).
	//
	// NOTE on spend: ShouldBid's actualSpend should be flight-to-date spend in
	// flight mode. The DSP supplies daily spend, which equals flight-to-date for
	// flights inside a single UTC day (the common/short case); the caller's
	// daily-budget exhaustion check bounds longer flights. An exact multi-day
	// lifetime-spend meter is a documented follow-up.
	FlightStart    time.Time
	FlightEnd      time.Time
	LifetimeBudget float64
}

// Pacer controls bid/no-bid decisions based on spend pacing.
type Pacer struct {
	clk    clock.Clock
	config Config
}

// New creates a Pacer with the given clock and config.
func New(clk clock.Clock, cfg Config) *Pacer {
	return &Pacer{clk: clk, config: cfg}
}

// flightAware reports whether this pacer spreads a lifetime budget over a flight
// window rather than a daily budget over a 24h day.
func (p *Pacer) flightAware() bool {
	return p.config.LifetimeBudget > 0 && p.config.FlightEnd.After(p.config.FlightStart)
}

// pacingBudget is the budget the pace curve is spread over: the flight lifetime
// budget in flight-aware mode, else the daily budget.
func (p *Pacer) pacingBudget() float64 {
	if p.flightAware() {
		return p.config.LifetimeBudget
	}
	return p.config.DailyBudget
}

// ShouldBid returns true if we should bid on this request.
// Uses probabilistic throttling based on pacing ratio.
func (p *Pacer) ShouldBid(actualSpend float64) bool {
	if p.config.Mode == ModeASAP {
		// ASAP: always bid until the budget (lifetime in flight mode) is exhausted
		return actualSpend < p.pacingBudget()
	}

	target := p.TargetSpend()
	if target <= 0 {
		return true // day hasn't started or no budget
	}

	ratio := actualSpend / target
	throttle := p.throttleRate(ratio)

	return rand.Float64() < throttle
}

// TargetSpend returns how much should have been spent by now.
func (p *Pacer) TargetSpend() float64 {
	elapsed := p.elapsedFraction()
	switch p.config.Mode {
	case ModeFrontLoaded:
		return p.pacingBudget() * frontLoadedCurve(elapsed)
	default: // even
		return p.pacingBudget() * elapsed
	}
}

// PacingRatio returns actual/target spend ratio.
// < 1.0 = behind pace, 1.0 = on pace, > 1.0 = ahead of pace.
func (p *Pacer) PacingRatio(actualSpend float64) float64 {
	target := p.TargetSpend()
	if target <= 0 {
		return 0
	}
	return actualSpend / target
}

// elapsedFraction returns what fraction of the pacing window has elapsed
// (0.0 to 1.0). The window is the flight [FlightStart, FlightEnd] in flight-aware
// mode, else the 24h day from DayStartUTC.
func (p *Pacer) elapsedFraction() float64 {
	now := p.clk.Now()
	start := p.config.DayStartUTC
	window := 24 * time.Hour
	if p.flightAware() {
		start = p.config.FlightStart
		window = p.config.FlightEnd.Sub(p.config.FlightStart)
	}
	elapsed := now.Sub(start)
	if elapsed < 0 {
		return 0
	}
	if window <= 0 {
		return 1.0
	}
	fraction := elapsed.Seconds() / window.Seconds()
	if fraction > 1.0 {
		return 1.0
	}
	return fraction
}

// throttleRate returns the probability of bidding (0.0 to 1.0)
// based on the pacing ratio.
func (p *Pacer) throttleRate(ratio float64) float64 {
	switch {
	case ratio < 0.8:
		return 1.0 // behind pace: bid on everything
	case ratio < 1.0:
		return 0.9 // slightly behind: bid on 90%
	case ratio < 1.2:
		return 0.7 // on pace: bid selectively
	case ratio < 1.5:
		return 0.3 // ahead: heavy throttle
	default:
		return 0.1 // far ahead: minimal bidding
	}
}

// frontLoadedCurve returns the target spend fraction for front-loaded mode.
// 80% spent by noon (0.5 of day), remaining 20% in afternoon.
func frontLoadedCurve(elapsed float64) float64 {
	if elapsed <= 0.5 {
		// First half of day: spend 80% of budget
		return elapsed * 2 * 0.8 // linear ramp to 0.8 at 0.5
	}
	// Second half: spend remaining 20%
	afternoonFraction := (elapsed - 0.5) * 2 // 0 to 1 for afternoon
	return 0.8 + afternoonFraction*0.2
}

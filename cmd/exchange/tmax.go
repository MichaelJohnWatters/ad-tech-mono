package main

import (
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// tmaxFloor keeps a pathological tmax (≤ margin) from producing a zero or
// negative fan-out deadline — the auction still gets a token window.
const tmaxFloor = 10 * time.Millisecond

// tmaxMarginFn is exchange.tmax_margin (live). Package-level so the auction
// handler reads it per request without growing its signature; set in main.
var tmaxMarginFn = func() time.Duration { return 25 * time.Millisecond }

// auctionDeadline is the DSP fan-out budget for one auction. OpenRTB tmax is
// the caller's WHOLE budget for our response, so the exchange keeps margin for
// its own post-fan-out work (dealeval/finalize/encode + the return hop) and
// never exceeds its configured bid_timeout. No tmax (0) = bid_timeout alone.
func auctionDeadline(bidTimeout time.Duration, tmaxMs int, margin time.Duration) time.Duration {
	if tmaxMs <= 0 {
		return bidTimeout
	}
	budget := time.Duration(tmaxMs)*time.Millisecond - margin
	if budget < tmaxFloor {
		budget = tmaxFloor
	}
	if bidTimeout > 0 && bidTimeout < budget {
		return bidTimeout
	}
	return budget
}

// channelTimeouts is the parsed exchange.bid_timeout_by_channel, cached by its
// raw string so the per-auction read is an atomic load + map lookup — the
// string is only re-parsed when an operator changes it.
type channelTimeouts struct {
	raw string
	m   map[string]time.Duration
	max time.Duration // largest override (sizes the HTTP client backstop)
}

var channelTimeoutCache atomic.Pointer[channelTimeouts]

// dspCallPerAuctionFn reads exchange.dsp_call_per_auction (live); set in main.
var dspCallPerAuctionFn = func() bool { return true }

// shadowProbeFn reads exchange.routing_shadow_probe (live); set in main.
var shadowProbeFn = func() bool { return false }

// channelTimeoutsRawFn reads exchange.bid_timeout_by_channel (live); set in
// main. Package-level like tmaxMarginFn.
var channelTimeoutsRawFn = func() string { return "" }

// knownChannel is every routing channel an auction can carry (a typo like
// "dispaly=200ms" would otherwise be silently inert).
var knownChannel = map[string]bool{
	constants.ChannelDisplay: true, constants.ChannelVideo: true, constants.ChannelAudio: true,
	constants.ChannelNative: true, constants.ChannelDOOH: true, constants.ChannelCTV: true,
	constants.ChannelRetail: true, constants.ChannelInGame: true,
}

// parseChannelTimeouts parses "display=250ms,video=600ms". Malformed or
// non-positive entries are skipped and reported in errs; the rest apply.
func parseChannelTimeouts(raw string) (map[string]time.Duration, []error) {
	m := map[string]time.Duration{}
	var errs []error
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ch, val, ok := strings.Cut(part, "=")
		ch = strings.ToLower(strings.TrimSpace(ch))
		if !ok || ch == "" {
			errs = append(errs, fmt.Errorf("entry %q: want channel=duration", part))
			continue
		}
		if !knownChannel[ch] {
			errs = append(errs, fmt.Errorf("entry %q: unknown channel %q", part, ch))
			continue
		}
		d, err := time.ParseDuration(strings.TrimSpace(val))
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("entry %q: invalid duration", part))
			continue
		}
		m[ch] = d
	}
	return m, errs
}

// loadChannelTimeouts returns the cached parse of raw, rebuilding (and logging
// any malformed entries once) when the raw value changed.
func loadChannelTimeouts(raw string, log *slog.Logger) *channelTimeouts {
	if ct := channelTimeoutCache.Load(); ct != nil && ct.raw == raw {
		return ct
	}
	m, errs := parseChannelTimeouts(raw)
	for _, err := range errs {
		if log != nil {
			log.Error("exchange.bid_timeout_by_channel: ignoring malformed entry", "error", err)
		}
	}
	ct := &channelTimeouts{raw: raw, m: m}
	for _, d := range m {
		if d > ct.max {
			ct.max = d
		}
	}
	channelTimeoutCache.Store(ct)
	return ct
}

// channelBidTimeout is the per-channel override for channel, else def.
func channelBidTimeout(ct *channelTimeouts, channel string, def time.Duration) time.Duration {
	if ct != nil {
		if d, ok := ct.m[channel]; ok {
			return d
		}
	}
	return def
}

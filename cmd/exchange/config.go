package main

import (
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// exchangeSchema is the exchange service's owned config keys. Passed into
// config.Setup at boot; the pod publishes the full schema (these entries +
// platform defaults) into its service_registry row. Adding a new
// auction-side knob = add an entry here.
var exchangeSchema = []config.SchemaEntry{
	{Key: "exchange.channel", Type: "string", Tier: config.TierLive, Default: "all", Description: "Restricts which inventory channels this exchange instance auctions for (all, display, video, ctv, native, audio). Lets you stand up channel-dedicated instances without changing code.", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.bid_timeout", Type: "duration", Tier: config.TierLive, Default: "500ms", Description: "Maximum time the exchange will wait for DSP bids before closing the auction. OpenRTB industry default is 100ms; we run 500ms locally because each DSP is a single Go process and concurrent load slows it.", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.dsp_endpoints", Type: "string", Tier: config.TierLive, Default: "http://localhost:8082,http://localhost:8089,http://localhost:8090", Description: "Comma-separated DSP base URLs the exchange fans bid requests out to. Smart routing further filters this list per auction. Live-tunable so e2e tests and ops can swap in test stubs or remove broken endpoints without restarting the exchange.", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.nats_url", Type: "string", Tier: config.TierStatic, Default: "nats://localhost:4222", Description: "NATS JetStream URL for publishing AuctionWinEvent and AuctionCompleteEvent.", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.max_retries", Type: "int", Tier: config.TierStatic, Default: "30", Description: "How many times the exchange retries binding its HTTP port on startup before exiting. Helps in Tilt where ports may still be in TIME_WAIT after a restart.", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "exchange.win_loss_enabled", Type: "bool", Tier: config.TierLive, Default: "true", Description: "Send win/loss notifications back to DSPs after auction completes. Disable to silence them in load tests or when investigating DSP-side feedback loops.", Service: constants.ServiceExchange, Since: "v1.0"},
	{Key: "cache.warm.deals.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "30s", Description: "How often the in-memory deal cache refreshes from Postgres. Affects how quickly newly activated PG/PMP deals start preempting open-market bids.", Service: constants.ServiceExchange, Since: "v1.1"},
	{Key: "prebid.min_bid_floor", Type: "float", Tier: config.TierLive, Default: "0", Description: "Platform-wide minimum CPM floor applied to inbound Prebid Server bid requests. Effective floor per impression = max(inbound bidfloor, this value). Raise to enforce a global quality floor on Prebid demand.", Service: constants.ServiceExchange, Since: "v1.3"},
	{Key: "prebid.enabled", Type: "bool", Tier: config.TierLive, Default: "true", Description: "Toggle the inbound Prebid bidder endpoint. Disable to stop accepting external Prebid Server traffic without restarting the exchange.", Service: constants.ServiceExchange, Since: "v1.3"},
}

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
		BidTimeout: config.NewLiveDuration(sc.Manager, sc.Cfg, "exchange.bid_timeout", 500*time.Millisecond),
	}
}

// Channel is the inventory-channel restriction for this exchange instance
// (all, display, video, ctv, native, audio). TierLive — read per auction.
func (k *Knobs) Channel() string { return k.cfg.Get("exchange.channel", constants.ChannelAll) }

// WinLossEnabled controls whether the exchange sends win/loss callbacks to
// DSPs. TierLive — read per auction.
func (k *Knobs) WinLossEnabled() bool { return k.cfg.GetBool("exchange.win_loss_enabled", true) }

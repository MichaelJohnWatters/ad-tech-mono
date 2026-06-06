package main

import (
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// trackerSchema is the tracker's owned config keys. Passed to config.Setup
// at boot; the pod writes the full schema (these + platform defaults) into
// its service_registry row.
var trackerSchema = []config.SchemaEntry{
	{Key: "tracker.nats_url", Type: "string", Tier: config.TierStatic, Default: "nats://localhost:4222", Description: "NATS JetStream URL where impression/click/conversion/view events are published.", Service: constants.ServiceTracker, Since: "v1.0"},
	{Key: "tracker.reporting_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8086", Description: "HTTP fallback URL for the reporting service. Used if NATS is unavailable so events still reach the analytics store.", Service: constants.ServiceTracker, Since: "v1.0"},
	{Key: "tracker.signing_key", Type: "string", Tier: config.TierSecret, Default: "adtech-dev-signing-key-change-in-prod", Description: "HMAC secret used to sign pixel URLs. Rotating this immediately invalidates every outstanding tracking URL — coordinate with ad server creative regeneration.", Service: constants.ServiceTracker, Since: "v1.0"},
	{Key: "tracker.fraud_enabled", Type: "bool", Tier: config.TierLive, Default: "true", Description: "Run real-time fraud checks (bot UA, IP blocklist, rate limit) before recording an event. Disable only for clean-room investigations.", Service: constants.ServiceTracker, Since: "v1.0"},
	{Key: "tracker.signature_validation", Type: "bool", Tier: config.TierLive, Default: "false", Description: "If true, requests without a valid HMAC sig param are rejected with 403. If false, signatures are logged as warnings but the event is still recorded — useful while phasing in signing.", Service: constants.ServiceTracker, Since: "v1.0"},
	{Key: "tracker.rate_limit_per_ip", Type: "int", Tier: config.TierLive, Default: "60", Description: "Maximum requests per IP per minute. Hitting the limit returns 429 and drops the event. Tune up for legitimate high-traffic publishers.", Service: constants.ServiceTracker, Since: "v1.0"},
	{Key: "tracker.dedup_ttl", Type: "duration", Tier: config.TierLive, Default: "24h", Description: "How long the Redis dedup key for an event stays alive. Duplicates within this window are silently dropped.", Service: constants.ServiceTracker, Since: "v1.1"},
	{Key: "tracker.dedup_enabled", Type: "bool", Tier: config.TierLive, Default: "true", Description: "Use Redis SetNX to deduplicate identical events (same event type + trace_id). Disable during incident replays where duplicates are expected.", Service: constants.ServiceTracker, Since: "v1.1"},
	{Key: "tracker.exp_validation", Type: "bool", Tier: config.TierLive, Default: "true", Description: "If true, reject requests whose exp=<unix-ts> param is in the past with 410 Gone. URLs without exp pass unchanged (legacy / unsigned dev calls). Disable for incident replay where stale URLs need to fire.", Service: constants.ServiceTracker, Since: "v1.2"},
	{Key: "cache.warm.fraud_rules.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "60s", Description: "How often the in-memory fraud-rule cache refreshes. Affects how quickly newly added IP/UA blocklist entries start taking effect.", Service: constants.ServiceTracker, Since: "v1.1"},
	{Key: "cache.warm.signing_keys.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "60s", Description: "How often the in-memory HMAC signing-key cache refreshes. Drives how quickly a rotated key becomes usable for signature validation.", Service: constants.ServiceTracker, Since: "v1.1"},
}

// Knobs is the tracker's typed config accessor. See cmd/dsp/config.go for
// the pattern explanation.
type Knobs struct {
	cfg *config.Config

	// Live (consumed inside Dedup constructor).
	DedupTTL     *config.LiveDuration
	DedupEnabled *config.LiveBool
}

func NewKnobs(sc *config.ServiceConfig) *Knobs {
	return &Knobs{
		cfg:          sc.Cfg,
		DedupTTL:     config.NewLiveDuration(sc.Manager, sc.Cfg, "tracker.dedup_ttl", 24*time.Hour),
		DedupEnabled: config.NewLiveBool(sc.Manager, sc.Cfg, "tracker.dedup_enabled", true),
	}
}

// FraudEnabled flips real-time fraud checks on/off. TierLive — read per pixel.
func (k *Knobs) FraudEnabled() bool { return k.cfg.GetBool("tracker.fraud_enabled", true) }

// SignatureValidation toggles strict HMAC enforcement. TierLive — read per pixel.
func (k *Knobs) SignatureValidation() bool { return k.cfg.GetBool("tracker.signature_validation", false) }

// RateLimitPerIP is the per-IP per-minute request ceiling. TierLive — read
// per pixel by the rate limiter.
func (k *Knobs) RateLimitPerIP() int { return k.cfg.GetInt("tracker.rate_limit_per_ip", 60) }

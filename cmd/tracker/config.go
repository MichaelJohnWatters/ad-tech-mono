package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

var trackerSchema = []config.SchemaEntry{
	{Key: "tracker.nats_url", Type: "string", Tier: config.TierStatic, Default: "nats://localhost:4222", Description: "NATS JetStream URL", Service: constants.ServiceTracker, Since: "v1.0"},
	{Key: "tracker.reporting_url", Type: "string", Tier: config.TierStatic, Default: "http://localhost:8086", Description: "Reporting service URL (HTTP fallback)", Service: constants.ServiceTracker, Since: "v1.0"},
	{Key: "tracker.signing_key", Type: "string", Tier: config.TierSecret, Default: "adtech-dev-signing-key-change-in-prod", Description: "HMAC signing key for pixel URLs", Service: constants.ServiceTracker, Since: "v1.0"},
	{Key: "tracker.fraud_enabled", Type: "bool", Tier: config.TierLive, Default: "true", Description: "Enable real-time fraud checks on pixel endpoints", Service: constants.ServiceTracker, Since: "v1.0"},
	{Key: "tracker.signature_validation", Type: "bool", Tier: config.TierLive, Default: "false", Description: "Enforce HMAC signature validation (false = warn only)", Service: constants.ServiceTracker, Since: "v1.0"},
	{Key: "tracker.rate_limit_per_ip", Type: "int", Tier: config.TierLive, Default: "60", Description: "Max requests per IP per minute before rate limiting", Service: constants.ServiceTracker, Since: "v1.0"},
	{Key: "tracker.dedup_ttl", Type: "duration", Tier: config.TierLive, Default: "24h", Description: "TTL for tracker SetNX dedup keys", Service: constants.ServiceTracker, Since: "v1.1"},
	{Key: "tracker.dedup_enabled", Type: "bool", Tier: config.TierLive, Default: "true", Description: "Enable Redis dedup on incoming events", Service: constants.ServiceTracker, Since: "v1.1"},
	{Key: "cache.warm.fraud_rules.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "60s", Description: "Poll interval for tracker fraud rule cache", Service: constants.ServiceTracker, Since: "v1.1"},
	{Key: "cache.warm.signing_keys.poll_interval", Type: "duration", Tier: config.TierStatic, Default: "60s", Description: "Poll interval for tracker HMAC signing key cache", Service: constants.ServiceTracker, Since: "v1.1"},
}

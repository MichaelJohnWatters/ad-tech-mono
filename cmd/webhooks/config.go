package main

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// webhooksSchema is the dispatcher's owned config keys. database.url is the
// shared platform key (registered by pkg/config).
var webhooksSchema = []config.SchemaEntry{
	{Key: "webhooks.port", Type: "string", Tier: config.TierStatic, Default: routes.PortWebhooks, Description: "HTTP port for the webhooks dispatcher's health/metrics endpoints.", Service: constants.ServiceWebhooks, Since: "v1.6"},
	{Key: "webhooks.nats_url", Type: "string", Tier: config.TierStatic, Default: routes.DefaultNATSURL, Description: "NATS JetStream URL the dispatcher consumes business events from.", Service: constants.ServiceWebhooks, Since: "v1.6"},
	{Key: "webhooks.max_attempts", Type: "int", Tier: config.TierLive, Default: "3", Description: "Max delivery attempts per subscription before giving up (each attempt is recorded in webhook_deliveries).", Service: constants.ServiceWebhooks, Since: "v1.6"},
	{Key: "webhooks.http_timeout", Type: "duration", Tier: config.TierLive, Default: "10s", Description: "Per-attempt HTTP timeout when POSTing to a receiver endpoint.", Service: constants.ServiceWebhooks, Since: "v1.6"},
	{Key: "webhooks.backoff_base", Type: "duration", Tier: config.TierLive, Default: "1s", Description: "Base backoff between delivery retries; grows exponentially (base, 2×base, 4×base…).", Service: constants.ServiceWebhooks, Since: "v1.6"},
}

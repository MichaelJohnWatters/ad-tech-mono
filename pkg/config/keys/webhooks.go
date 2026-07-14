package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var webhooksSet = config.NewKeySet(constants.ServiceWebhooks)

// WebhooksSchema is the Webhooks schema — passed to config.Setup at boot.
func WebhooksSchema() []config.SchemaEntry { return webhooksSet.Entries() }

// Webhooks holds the Webhooks config keys.
var Webhooks = struct {
	Port        config.StringKey
	NATSURL     config.StringKey
	MaxAttempts config.IntKey
	HttpTimeout config.DurationKey
	BackoffBase config.DurationKey
}{
	Port:        webhooksSet.String("webhooks.port", routes.PortWebhooks, config.TierStatic, "HTTP port for the webhooks dispatcher's health/metrics endpoints.", config.Since("v1.6")),
	NATSURL:     webhooksSet.String("webhooks.nats_url", routes.DefaultNATSURL, config.TierStatic, "NATS JetStream URL the dispatcher consumes business events from.", config.Since("v1.6")),
	MaxAttempts: webhooksSet.Int("webhooks.max_attempts", "3", config.TierLive, "Max delivery attempts per subscription before giving up (each attempt is recorded in webhook_deliveries).", config.Since("v1.6")),
	HttpTimeout: webhooksSet.Duration("webhooks.http_timeout", "10s", config.TierLive, "Per-attempt HTTP timeout when POSTing to a receiver endpoint.", config.Since("v1.6")),
	BackoffBase: webhooksSet.Duration("webhooks.backoff_base", "1s", config.TierLive, "Base backoff between delivery retries; grows exponentially (base, 2×base, 4×base…).", config.Since("v1.6")),
}

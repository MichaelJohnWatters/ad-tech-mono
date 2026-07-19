package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

var notificationsSet = config.NewKeySet(constants.ServiceNotifications)

// NotificationsSchema is the Notifications schema — passed to config.Setup at boot.
func NotificationsSchema() []config.SchemaEntry { return notificationsSet.Entries() }

// Notifications holds the notifications consumer's config keys.
var Notifications = struct {
	Port    config.StringKey
	NATSURL config.StringKey
}{
	Port:    notificationsSet.String("notifications.port", routes.PortNotifications, config.TierStatic, "HTTP port for the notifications consumer's health/metrics endpoints.", config.Since("v1.10")),
	NATSURL: notificationsSet.String("notifications.nats_url", routes.DefaultNATSURL, config.TierStatic, "NATS JetStream URL the consumer reads account-scoped business events from.", config.Since("v1.10")),
}

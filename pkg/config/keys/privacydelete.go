package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// PrivacyDelete holds cmd/privacy-delete's keys. Raw: the binary registers
// no schema (nil Setup).
var PrivacyDelete = struct {
	NATSURL config.StringKey
}{
	NATSURL: config.RawString("privacy_delete.nats_url", routes.DefaultNATSURL),
}

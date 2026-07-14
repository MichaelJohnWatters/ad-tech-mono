package keys

import (
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
)

// Audience holds the shared audience-segment cache knobs read by the DSP and
// SSP. Raw: never registered in any schema — promote to a KeySet if they
// should become UI-tunable.
var Audience = struct {
	PreloadInterval config.DurationKey
	CacheTTL        config.DurationKey
}{
	PreloadInterval: config.RawDuration("audience.preload_interval", 30*time.Second),
	CacheTTL:        config.RawDuration("audience.cache_ttl", 90*time.Second),
}

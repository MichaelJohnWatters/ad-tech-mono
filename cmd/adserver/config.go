package main

import (
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
)

// Knobs is the ad server's typed config accessor. See cmd/dsp/config.go
// for the pattern explanation.
type Knobs struct {
	cfg *config.Config

	// Live (consumed inside FreqCap / CreativeResolver constructors).
	FreqCapLimit  *config.LiveInt
	FreqCapWindow *config.LiveDuration
	CreativeTTL   *config.LiveDuration
	URLTTL        *config.LiveDuration
}

func NewKnobs(sc *config.ServiceConfig) *Knobs {
	return &Knobs{
		cfg:           sc.Cfg,
		FreqCapLimit:  config.NewLiveInt(sc.Manager, sc.Cfg, keys.AdServer.FreqCapPerUserPerCampaign.Key(), 5),
		FreqCapWindow: config.NewLiveDuration(sc.Manager, sc.Cfg, keys.AdServer.FreqCapWindow.Key(), 5*time.Minute),
		CreativeTTL:   config.NewLiveDuration(sc.Manager, sc.Cfg, keys.AdServer.DefaultCreativeTTL.Key(), 5*time.Minute),
		URLTTL:        config.NewLiveDuration(sc.Manager, sc.Cfg, keys.AdServer.URLTTL.Key(), time.Hour),
	}
}

// BanditEnabled toggles Thompson-sampling creative rotation. TierLive —
// read per serve request so flipping the flag takes effect immediately.
func (k *Knobs) BanditEnabled() bool { return keys.AdServer.BanditEnabled.Get(k.cfg) }

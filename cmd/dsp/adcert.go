package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adcert"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

// adCertVerifierFn returns a per-request check on the ads.cert signature of an
// inbound bid request, mirroring the exchange's schain gate. keyFn supplies the
// current verification key (either a fetched key or the statically-configured
// one). The enforcement mode is read live so ops can ratchet off → warn →
// strict without a restart:
//
//   - off:    always allow, no check.
//   - warn:   verify; on a missing/invalid/stale signature log and bid anyway.
//   - strict: verify; on a missing/invalid/stale signature no-bid.
//
// Verification is also skipped (allow) when no key is available, so the feature
// stays inert until a key is configured or fetched.
func adCertVerifierFn(cfg *config.Config, log *slog.Logger, now func() time.Time, keysFn func() []ed25519.PublicKey) func(*openrtb.BidRequest) (bool, string) {
	return func(req *openrtb.BidRequest) (bool, string) {
		mode := strings.ToLower(strings.TrimSpace(keys.DSP.AdCertEnforcement.Get(cfg)))
		pubs := keysFn()
		if mode == "" || mode == "off" || len(pubs) == 0 {
			return true, ""
		}
		var sig string
		if req.Source != nil && req.Source.Ext != nil {
			sig = req.Source.Ext.AdCert
		}
		// Both must hold: a valid signature under ANY current key (overlap window)
		// AND a fresh timestamp (replay protection). maxAge <= 0 disables freshness.
		maxAge := keys.DSP.AdCertMaxAge.Get(cfg)
		sigOK := adcert.VerifyAny(pubs, req, sig)
		fresh := adcert.Fresh(req, now(), maxAge)
		if sigOK && fresh {
			return true, ""
		}
		reason := "adcert_invalid"
		if sigOK && !fresh {
			reason = "adcert_stale"
		}
		if mode == "strict" {
			return false, reason
		}
		log.Warn("adcert check failed (warn mode, bidding anyway)", "reason", reason, "trace_id", req.ID)
		return true, ""
	}
}

// adCertKeySource resolves the verification key. When dsp.adcert_key_url is set
// the exchange's published key is fetched and periodically refreshed (so key
// rotations propagate without a restart); otherwise the static
// dsp.adcert_verify_key is used. The returned func always prefers a freshly
// fetched key and falls back to the static one until the first fetch succeeds.
func adCertKeySource(cfg *config.Config, log *slog.Logger, onShutdown func(name string, fn func())) func() []ed25519.PublicKey {
	var static []ed25519.PublicKey
	if staticPub, err := adcert.ParsePublicKey(keys.DSP.AdCertVerifyKey.Get(cfg)); err != nil {
		log.Error("adcert: invalid static verify key", "error", err)
	} else if staticPub != nil {
		static = []ed25519.PublicKey{staticPub}
	}
	url := keys.DSP.AdCertKeyURL.Get(cfg)
	if url == "" {
		return func() []ed25519.PublicKey { return static }
	}
	f := newAdCertKeyFetcher(url, keys.DSP.AdCertKeyRefresh.Get(cfg), log)
	f.Start()
	onShutdown("adcert-key-fetcher", f.Stop)
	return func() []ed25519.PublicKey {
		if ks := f.Current(); len(ks) > 0 {
			return ks
		}
		return static
	}
}

// adCertKeyFetcher fetches the exchange's ads.cert public KEYSET on an interval,
// holding the latest set in an atomic pointer for lock-free reads on the bid
// path. The keyset lets a DSP verify against the active AND rotating keys during
// a grace window (overlap).
type adCertKeyFetcher struct {
	url      string
	interval time.Duration
	client   *http.Client
	log      *slog.Logger
	keys     atomic.Pointer[[]ed25519.PublicKey]
	stop     chan struct{}
}

func newAdCertKeyFetcher(url string, interval time.Duration, log *slog.Logger) *adCertKeyFetcher {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &adCertKeyFetcher{
		url:      url,
		interval: interval,
		client:   &http.Client{Timeout: 5 * time.Second},
		log:      log,
		stop:     make(chan struct{}),
	}
}

// Start does a best-effort initial fetch then refreshes on the interval.
func (f *adCertKeyFetcher) Start() {
	f.fetch()
	go func() {
		t := time.NewTicker(f.interval)
		defer t.Stop()
		for {
			select {
			case <-f.stop:
				return
			case <-t.C:
				f.fetch()
			}
		}
	}()
}

func (f *adCertKeyFetcher) Stop() { close(f.stop) }

// Current returns the latest fetched keyset, or nil before the first success.
func (f *adCertKeyFetcher) Current() []ed25519.PublicKey {
	if p := f.keys.Load(); p != nil {
		return *p
	}
	return nil
}

func (f *adCertKeyFetcher) fetch() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		f.log.Error("adcert: build key fetch request failed", "error", err)
		return
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.log.Error("adcert: key fetch failed (keeping last key)", "url", f.url, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.log.Error("adcert: key endpoint returned non-200 (keeping last key)", "status", resp.StatusCode)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	// Keyset shape (JWKS-style) with `keys`; fall back to the legacy single `key`.
	var kr struct {
		Alg  string `json:"alg"`
		Key  string `json:"key"`
		Keys []struct {
			Alg string `json:"alg"`
			Key string `json:"key"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &kr); err != nil {
		f.log.Error("adcert: key response decode failed", "error", err)
		return
	}
	seen := map[string]bool{}
	var pubs []ed25519.PublicKey
	add := func(b64 string) {
		if b64 == "" || seen[b64] {
			return
		}
		if pub, err := adcert.ParsePublicKey(b64); err == nil && pub != nil {
			seen[b64] = true
			pubs = append(pubs, pub)
		}
	}
	for _, k := range kr.Keys {
		add(k.Key)
	}
	add(kr.Key) // legacy single-key field / active
	if len(pubs) == 0 {
		f.log.Error("adcert: fetched keyset had no valid keys (keeping last)")
		return
	}
	f.keys.Store(&pubs)
}

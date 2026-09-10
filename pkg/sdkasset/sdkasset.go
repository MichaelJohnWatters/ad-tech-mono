// Package sdkasset loads the embeddable adtech.js SDK and derives the metadata the
// gateway needs to serve it under versioned, cache-friendly URLs (PLAN #106): the
// semantic version (parsed from the SDK's own SDK_VERSION constant), the major
// channel, an SRI integrity hash, a content ETag, and the per-channel Cache-Control.
//
// Serving model:
//   - /sdk/<exact>/adtech.js   (e.g. 2.0.0) — the bytes never change → immutable,
//     1-year cache. Safe to pin with Subresource Integrity.
//   - /sdk/v<major>/adtech.js  (e.g. v2)    — the stable line; receives backwards-
//     compatible patches → short cache, NO SRI (a patch would break a pinned hash).
//   - /sdk/latest/adtech.js                 — floats across majors → shortest cache.
package sdkasset

import (
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// versionRe pulls the semver out of `var SDK_VERSION = '2.0.0';` (single or double
// quotes) — the SDK is the source of truth for its own version.
var versionRe = regexp.MustCompile(`SDK_VERSION\s*=\s*['"]([0-9]+\.[0-9]+\.[0-9]+)['"]`)

// Asset is a loaded SDK build plus its derived serving metadata.
type Asset struct {
	Version   string // semver, e.g. "2.0.0"
	Major     string // channel, e.g. "v2"
	Bytes     []byte // the SDK source bytes served verbatim
	Integrity string // SRI hash, e.g. "sha384-…" (for pinned/exact URLs only)
	ETag      string // strong, quoted content ETag
}

// Load reads and parses the SDK from a file path.
func Load(path string) (*Asset, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return New(b)
}

// New derives the serving metadata from SDK source bytes.
func New(b []byte) (*Asset, error) {
	m := versionRe.FindSubmatch(b)
	if m == nil {
		return nil, fmt.Errorf("sdkasset: SDK_VERSION not found in %d bytes", len(b))
	}
	ver := string(m[1])
	sum := sha512.Sum384(b)
	return &Asset{
		Version:   ver,
		Major:     "v" + strings.SplitN(ver, ".", 2)[0],
		Bytes:     b,
		Integrity: "sha384-" + base64.StdEncoding.EncodeToString(sum[:]),
		ETag:      `"` + ver + "-" + base64.RawURLEncoding.EncodeToString(sum[:8]) + `"`,
	}, nil
}

// Channel is a serving channel with its own cache policy.
type Channel int

const (
	ChannelPinned Channel = iota // exact version → immutable
	ChannelMajor                 // vN → patched within the major line
	ChannelLatest                // floats across majors
)

// CacheControl is the Cache-Control header value for a channel. Pinned is immutable
// (the bytes at an exact-version URL never change); the major line gets a 1h TTL so
// backwards-compatible patches propagate; latest gets the shortest TTL.
func CacheControl(c Channel) string {
	switch c {
	case ChannelPinned:
		return "public, max-age=31536000, immutable"
	case ChannelMajor:
		return "public, max-age=3600"
	default:
		return "public, max-age=300"
	}
}

// Resolve maps a URL version segment to the channel THIS asset serves it under, or
// (_, false) when we don't host that version (→ 404, honest: a request for an
// exact version we no longer ship is not silently served the current build).
func (a *Asset) Resolve(seg string) (Channel, bool) {
	switch seg {
	case "latest":
		return ChannelLatest, true
	case a.Major:
		return ChannelMajor, true
	case a.Version:
		return ChannelPinned, true
	}
	return 0, false
}

// PinnedURL / MajorURL / LatestURL are the canonical serving paths.
func (a *Asset) PinnedURL() string { return "/sdk/" + a.Version + "/adtech.js" }
func (a *Asset) MajorURL() string  { return "/sdk/" + a.Major + "/adtech.js" }
func (a *Asset) LatestURL() string { return "/sdk/latest/adtech.js" }

// PinnedRef is the immutable exact-version URL together with the SRI hash valid
// for it. The integrity is scoped HERE (not top-level) on purpose: it is valid ONLY
// for this immutable URL — the major/latest channels receive patches, so applying
// this hash to them would break the script on the next patch.
type PinnedRef struct {
	URL       string `json:"url"`
	Integrity string `json:"integrity"`
}

// Metadata is the public /sdk/version.json contract. One source of truth for the
// wire shape (handler + any future CDN-manifest job share it).
type Metadata struct {
	Version   string    `json:"version"`
	Major     string    `json:"major"`
	Pinned    PinnedRef `json:"pinned"`     // immutable + SRI-safe
	MajorURL  string    `json:"major_url"`  // stable line, patched, NO SRI
	LatestURL string    `json:"latest_url"` // floats, NO SRI
}

// Metadata builds the version.json payload for this asset.
func (a *Asset) Metadata() Metadata {
	return Metadata{
		Version:   a.Version,
		Major:     a.Major,
		Pinned:    PinnedRef{URL: a.PinnedURL(), Integrity: a.Integrity},
		MajorURL:  a.MajorURL(),
		LatestURL: a.LatestURL(),
	}
}

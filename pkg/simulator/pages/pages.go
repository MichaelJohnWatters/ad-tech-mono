// Package pages is the SINGLE SOURCE OF TRUTH for the external demo publisher's
// multi-slot page layouts. A page is just an ordered list of ad slots, each with
// a format and a seeded placement key.
//
// Two consumers read these layouts and MUST agree on them:
//
//   - cmd/demosite renders each layout as a real browser page — every slot fires
//     the matching /v1/pubad/* call cross-origin, exactly as a third-party site.
//   - tests/e2e replays each layout without a browser (VisitPage): for every slot
//     it serves the ad and fires the impression/viewability beacons, then asserts
//     the per-format counts that reach ClickHouse equal CountByFormat().
//
// Because both sides import THIS package, "the page has N ads of these formats"
// is defined once. If the e2e asserts N impressions downstream, that is the same
// N the real page requests — no drift, no lie. (Same discipline as
// pkg/simulator/request, which sim_test.go proves the web UI and harness share.)
package pages

// Format is an ad format. The string value is the /v1/pubad/* channel and the
// analytics `channel` column, so it flows unchanged from slot → serve → beacon →
// ClickHouse.
type Format string

const (
	Display Format = "display"
	Native  Format = "native"
	Video   Format = "video"
	Audio   Format = "audio"
)

// Slot is one ad placement on a page. PlacementKey is a seeded placement's
// external id (profiles/publishers/standard.yaml). Width/Height are the rendered
// box; zero means format-default (native/audio have no fixed box).
type Slot struct {
	Format       Format
	PlacementKey string
	Label        string // human label shown above the slot ("Advertisement — MPU")
	Width        int
	Height       int
}

// Layout is a named page: an ordered set of slots plus presentation copy.
type Layout struct {
	Slug  string // URL segment: /p/{slug} on the demosite, and the e2e lookup key
	Title string // <title> / nav label
	Desc  string // one-line editorial description
	Slots []Slot
}

// CountByFormat returns how many slots of each format the page carries — the
// exact number of impressions (and, for display/video, viewability views) that
// a full visit must land downstream. This is the e2e's assertion target.
func (l Layout) CountByFormat() map[Format]int {
	out := make(map[Format]int, 4)
	for _, s := range l.Slots {
		out[s.Format]++
	}
	return out
}

// Total is the number of ad slots on the page.
func (l Layout) Total() int { return len(l.Slots) }

// Seeded placement external keys (profiles/publishers/standard.yaml, pub-simulator).
// One per format; a page reuses a key across multiple same-format slots, which is
// realistic (a site runs several MPUs off one placement definition).
const (
	placeDisplay = "pl-sim-mpu"
	placeNative  = "pl-sim-native"
	placeVideo   = "pl-sim-video"
	placeAudio   = "pl-sim-audio"
)

// layouts is the registry. Combos deliberately vary slot count and format mix so
// the e2e exercises single-format pages, mixed pages, and a heavy 6-ad page.
var layouts = []Layout{
	{
		Slug:  "news-3ad",
		Title: "Breaking: three-ad news page",
		Desc:  "A standard news article — leaderboard + MPU display units and one in-feed native.",
		Slots: []Slot{
			{Format: Display, PlacementKey: placeDisplay, Label: "Leaderboard", Width: 728, Height: 90},
			{Format: Display, PlacementKey: placeDisplay, Label: "MPU", Width: 300, Height: 250},
			{Format: Native, PlacementKey: placeNative, Label: "In-feed native"},
		},
	},
	{
		Slug:  "longread-6ad",
		Title: "Long read: six-ad mixed page",
		Desc:  "A feature article running every format — three display units, native, a video pre-roll and an audio spot.",
		Slots: []Slot{
			{Format: Display, PlacementKey: placeDisplay, Label: "Leaderboard", Width: 728, Height: 90},
			{Format: Display, PlacementKey: placeDisplay, Label: "MPU (sidebar)", Width: 300, Height: 250},
			{Format: Display, PlacementKey: placeDisplay, Label: "MPU (in-content)", Width: 300, Height: 250},
			{Format: Native, PlacementKey: placeNative, Label: "In-feed native"},
			{Format: Video, PlacementKey: placeVideo, Label: "Video pre-roll", Width: 640, Height: 360},
			{Format: Audio, PlacementKey: placeAudio, Label: "Audio spot"},
		},
	},
	{
		Slug:  "video-hub",
		Title: "Video hub",
		Desc:  "A video-first page: a pre-roll player flanked by two companion display units.",
		Slots: []Slot{
			{Format: Video, PlacementKey: placeVideo, Label: "Player pre-roll", Width: 640, Height: 360},
			{Format: Display, PlacementKey: placeDisplay, Label: "Companion (top)", Width: 300, Height: 250},
			{Format: Display, PlacementKey: placeDisplay, Label: "Companion (bottom)", Width: 300, Height: 250},
		},
	},
	{
		Slug:  "news-home",
		Title: "Front page",
		Desc:  "A busy news homepage: top leaderboard, two sidebar MPUs, three in-feed native units and a video story.",
		Slots: []Slot{
			{Format: Display, PlacementKey: placeDisplay, Label: "Top leaderboard", Width: 728, Height: 90},
			{Format: Native, PlacementKey: placeNative, Label: "Lead story (native)"},
			{Format: Display, PlacementKey: placeDisplay, Label: "Sidebar MPU (top)", Width: 300, Height: 250},
			{Format: Native, PlacementKey: placeNative, Label: "In-feed native"},
			{Format: Video, PlacementKey: placeVideo, Label: "Video story", Width: 640, Height: 360},
			{Format: Display, PlacementKey: placeDisplay, Label: "Sidebar MPU (bottom)", Width: 300, Height: 250},
			{Format: Native, PlacementKey: placeNative, Label: "More stories (native)"},
		},
	},
	{
		Slug:  "feed-8ad",
		Title: "Infinite feed",
		Desc:  "An eight-ad social/blog feed: native cards interleaved with display units and an in-feed video.",
		Slots: []Slot{
			{Format: Native, PlacementKey: placeNative, Label: "Feed card 1"},
			{Format: Display, PlacementKey: placeDisplay, Label: "Feed MPU 1", Width: 300, Height: 250},
			{Format: Native, PlacementKey: placeNative, Label: "Feed card 2"},
			{Format: Video, PlacementKey: placeVideo, Label: "In-feed video", Width: 640, Height: 360},
			{Format: Native, PlacementKey: placeNative, Label: "Feed card 3"},
			{Format: Display, PlacementKey: placeDisplay, Label: "Feed MPU 2", Width: 300, Height: 250},
			{Format: Native, PlacementKey: placeNative, Label: "Feed card 4"},
			{Format: Display, PlacementKey: placeDisplay, Label: "Feed MPU 3", Width: 300, Height: 250},
		},
	},
	{
		Slug:  "all-formats",
		Title: "One of everything",
		Desc:  "A single page carrying exactly one slot of each format — the minimal full-coverage combo.",
		Slots: []Slot{
			{Format: Display, PlacementKey: placeDisplay, Label: "Display MPU", Width: 300, Height: 250},
			{Format: Native, PlacementKey: placeNative, Label: "Native"},
			{Format: Video, PlacementKey: placeVideo, Label: "Video pre-roll", Width: 640, Height: 360},
			{Format: Audio, PlacementKey: placeAudio, Label: "Audio spot"},
		},
	},
}

// All returns every registered layout in stable order.
func All() []Layout { return layouts }

// BySlug looks up a layout by its URL slug.
func BySlug(slug string) (Layout, bool) {
	for _, l := range layouts {
		if l.Slug == slug {
			return l, true
		}
	}
	return Layout{}, false
}

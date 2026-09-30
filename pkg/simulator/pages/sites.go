package pages

// A Site is one external publisher property — a "friend's website". It groups
// branding, the tenant it maps to (publisher external key + revenue share), the
// domain it's served on, and which page layouts it runs. Each site owns its OWN
// placements (one per format) so impressions attribute to the right publisher
// tenant; PlacementByFormat is the format→placement-key map both the demosite
// (renders the site) and the e2e (seeds + replays it) use.
//
// Same single-source-of-truth discipline as Layout: "these three sites, with
// these pages and these placements" is defined once here, so a browser visit
// and the multi-tenant e2e can't disagree about what should be served or where
// it should land.
type Site struct {
	Slug        string // URL/deploy id: "chronicle"
	Name        string // masthead: "The Daily Chronicle"
	Tagline     string
	Kind        string // "news" | "blog" | "video" — drives editorial copy
	Publisher   string // publisher tenant external key this site maps to
	Domain      string // the site's own origin (for realism + CORS)
	RevsharePct int    // the publisher's cut of gross (validated per-tenant in e2e)
	LayoutSlugs []string
}

// PlacementByFormat is this site's own placement external keys, one per format,
// derived from the slug so they never collide across sites (chronicle-display,
// gadget-video, …). The e2e seeds exactly these; the demosite requests them.
func (s Site) PlacementByFormat() map[Format]string {
	return map[Format]string{
		Display: s.Slug + "-display",
		Native:  s.Slug + "-native",
		Video:   s.Slug + "-video",
		Audio:   s.Slug + "-audio",
	}
}

// Layouts resolves this site's LayoutSlugs to the shared Layout definitions.
func (s Site) Layouts() []Layout {
	out := make([]Layout, 0, len(s.LayoutSlugs))
	for _, slug := range s.LayoutSlugs {
		if l, ok := BySlug(slug); ok {
			out = append(out, l)
		}
	}
	return out
}

// sites is the registry of external publisher properties. Three distinct
// tenants with different content mixes, revenue shares, and page weights — the
// spread the multi-tenant e2e asserts isolation and correct money across.
var sites = []Site{
	{
		Slug: "chronicle", Name: "The Daily Chronicle", Tagline: "News, as it breaks",
		Kind: "news", Publisher: "pub-chronicle", Domain: "chronicle.example",
		RevsharePct: 70,
		LayoutSlugs: []string{"news-home", "longread-6ad", "news-3ad"},
	},
	{
		Slug: "gadget", Name: "Gadget Grotto", Tagline: "Reviews, teardowns, and deals",
		Kind: "blog", Publisher: "pub-gadget", Domain: "gadgetgrotto.example",
		RevsharePct: 65,
		LayoutSlugs: []string{"feed-8ad", "all-formats"},
	},
	{
		Slug: "primereel", Name: "PrimeReel", Tagline: "Stream the good stuff",
		Kind: "video", Publisher: "pub-primereel", Domain: "primereel.example",
		RevsharePct: 80,
		LayoutSlugs: []string{"video-hub", "all-formats", "longread-6ad"},
	},
	{
		// Spotify-style audio/music app — audio-only. Its only page is the
		// audio player (no display/native/video layouts) so the branded origin
		// serves DAAST audio exclusively.
		Slug: "soundwave", Name: "SoundWave", Tagline: "Music, podcasts, and audio ads",
		Kind: "audio", Publisher: "pub-soundwave", Domain: "soundwave.example",
		RevsharePct: 75,
		LayoutSlugs: []string{"audio-only"},
	},
	{
		// Twitch-style live/CTV streaming — SSAI-only. Its page is the live
		// stitched-stream player (server-side ad insertion), no client VAST.
		Slug: "twitchr", Name: "Twitchr", Tagline: "Live streams, stitched ads",
		Kind: "ssai", Publisher: "pub-twitchr", Domain: "twitchr.example",
		RevsharePct: 70,
		LayoutSlugs: []string{"ssai-live"},
	},
	{
		// YouTube-style UGC video — client-side VAST (the player inserts the
		// pre-roll itself, the counterpoint to Twitchr's server-side SSAI).
		Slug: "viewtube", Name: "ViewTube", Tagline: "Watch, upload, pre-roll",
		Kind: "video", Publisher: "pub-viewtube", Domain: "viewtube.example",
		RevsharePct: 68,
		LayoutSlugs: []string{"video-hub", "feed-8ad"},
	},
}

// AllSites returns every registered site in stable order.
func AllSites() []Site { return sites }

// SiteBySlug looks up a site by slug.
func SiteBySlug(slug string) (Site, bool) {
	for _, s := range sites {
		if s.Slug == slug {
			return s, true
		}
	}
	return Site{}, false
}

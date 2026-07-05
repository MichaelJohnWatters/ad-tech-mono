package main

import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"

// buildVideoImp constructs the video object for a bid request's impression,
// starting from the platform's standard pre-roll defaults and overriding only
// the keys present in the placement's video_config. An empty/nil config yields
// the defaults unchanged, so existing video placements are untouched.
//
// Config keys (all optional): skippable (bool), skip_after (int seconds),
// min_duration, max_duration (int seconds), plcmt (int, OpenRTB 2.6 placement),
// linearity (int), w, h (int), mimes ([]string), protocols ([]int).
func buildVideoImp(cfg map[string]any) *openrtb.Video {
	v := &openrtb.Video{
		Mimes:       []string{"video/mp4", "video/webm"},
		Protocols:   []int{2, 3, 5, 6, 7}, // VAST 2-4.2
		W:           640,
		H:           360,
		MinDuration: 5,
		MaxDuration: 30,
		Linearity:   1, // linear (pre/mid/post-roll)
		Plcmt:       1, // instream with audio
		Skip:        1,
		SkipAfter:   5,
		SkipMin:     5,
		API:         []int{7}, // OMID 1
	}
	if len(cfg) == 0 {
		return v
	}
	if b, ok := cfgBool(cfg, "skippable"); ok {
		if b {
			v.Skip = 1
		} else {
			v.Skip, v.SkipAfter, v.SkipMin = 0, 0, 0
		}
	}
	if n, ok := cfgInt(cfg, "skip_after"); ok {
		v.SkipAfter, v.SkipMin = n, n
	}
	if n, ok := cfgInt(cfg, "min_duration"); ok {
		v.MinDuration = n
	}
	if n, ok := cfgInt(cfg, "max_duration"); ok {
		v.MaxDuration = n
	}
	if n, ok := cfgInt(cfg, "plcmt"); ok {
		v.Plcmt = n
	}
	if n, ok := cfgInt(cfg, "linearity"); ok {
		v.Linearity = n
	}
	if n, ok := cfgInt(cfg, "w"); ok {
		v.W = n
	}
	if n, ok := cfgInt(cfg, "h"); ok {
		v.H = n
	}
	if s, ok := cfgStrings(cfg, "mimes"); ok {
		v.Mimes = s
	}
	if p, ok := cfgInts(cfg, "protocols"); ok {
		v.Protocols = p
	}
	return v
}

// cfg* helpers read a typed value from a JSONB-decoded map (numbers arrive as
// float64, arrays as []any). The ok result is false when the key is absent or
// the wrong type, so a malformed value leaves the default in place.
func cfgBool(m map[string]any, k string) (bool, bool) {
	b, ok := m[k].(bool)
	return b, ok
}

func cfgInt(m map[string]any, k string) (int, bool) {
	switch n := m[k].(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

func cfgStrings(m map[string]any, k string) ([]string, bool) {
	raw, ok := m[k].([]any)
	if !ok || len(raw) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func cfgInts(m map[string]any, k string) ([]int, bool) {
	raw, ok := m[k].([]any)
	if !ok || len(raw) == 0 {
		return nil, false
	}
	out := make([]int, 0, len(raw))
	for _, x := range raw {
		if f, ok := x.(float64); ok {
			out = append(out, int(f))
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

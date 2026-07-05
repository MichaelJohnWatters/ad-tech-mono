// Package floors resolves the effective per-request floor price for a
// placement from its base floor plus optional device/geo/time overrides
// stored in placements.floor_config (JSONB).
//
// Config shape (all keys optional):
//
//	{
//	  "device":   {"mobile": 1.5, "desktop": 2.0},
//	  "geo":      {"USA": 2.5, "GBR": 1.8},
//	  "timezone": "America/New_York",
//	  "dayparts": [
//	    {"days": [1,2,3,4,5], "start_hour": 9, "end_hour": 18, "floor": 3.0},
//	    {"days": [0,6],       "start_hour": 20, "end_hour": 2, "floor": 4.0}
//	  ]
//	}
//
// Resolution is publisher-favouring: the effective floor is the MAX of the
// base floor and every override that matches the request (a request that is
// both mobile and in the USA during a matching daypart takes the highest of
// them all). An empty/nil config returns the base floor unchanged, so
// existing placements are untouched.
//
// Dayparts are evaluated in the config's "timezone" (IANA name; UTC if empty
// or unparseable). "days" are weekdays with Sunday=0 … Saturday=6 (Go's
// time.Weekday); an empty/omitted "days" matches every day. The hour window
// is [start_hour, end_hour); if end_hour <= start_hour the window wraps past
// midnight (e.g. 20→2 covers 20:00–01:59).
package floors

import (
	"strings"
	"time"
)

// Effective returns the floor to advertise for a request on a placement.
// device is matched case-insensitively; geo is matched case-insensitively
// against the country-code keys; at is the request time used for daypart
// matching (the caller passes time.Now()).
func Effective(base float64, config map[string]any, device, geo string, at time.Time) float64 {
	floor := base
	if config == nil {
		return floor
	}
	if v, ok := lookup(config["device"], device); ok && v > floor {
		floor = v
	}
	if v, ok := lookup(config["geo"], geo); ok && v > floor {
		floor = v
	}
	if v, ok := daypartFloor(config, at); ok && v > floor {
		floor = v
	}
	return floor
}

// daypartFloor returns the highest matching daypart floor for the request
// time, and whether any daypart matched. The evaluation clock is shifted into
// the config's timezone first.
func daypartFloor(config map[string]any, at time.Time) (float64, bool) {
	raw, ok := config["dayparts"].([]any)
	if !ok || len(raw) == 0 {
		return 0, false
	}
	local := at
	if tz, ok := config["timezone"].(string); ok && tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			local = at.In(loc)
		}
	}
	weekday := int(local.Weekday())
	hour := local.Hour()

	best, found := 0.0, false
	for _, item := range raw {
		dp, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if !dayMatches(dp["days"], weekday) {
			continue
		}
		if !hourMatches(dp["start_hour"], dp["end_hour"], hour) {
			continue
		}
		f, ok := toFloat(dp["floor"])
		if !ok {
			continue
		}
		if !found || f > best {
			best, found = f, true
		}
	}
	return best, found
}

// dayMatches reports whether weekday is in the days list. An empty/omitted or
// non-list value matches every day.
func dayMatches(days any, weekday int) bool {
	list, ok := days.([]any)
	if !ok || len(list) == 0 {
		return true
	}
	for _, d := range list {
		if n, ok := toFloat(d); ok && int(n) == weekday {
			return true
		}
	}
	return false
}

// hourMatches reports whether hour falls in [start, end). If end <= start the
// window wraps past midnight. Missing bounds default to a full day (match).
func hourMatches(startRaw, endRaw any, hour int) bool {
	start, sOK := toFloat(startRaw)
	end, eOK := toFloat(endRaw)
	if !sOK || !eOK {
		return true
	}
	s, e := int(start), int(end)
	if s == e {
		return true // degenerate full-day window
	}
	if s < e {
		return hour >= s && hour < e
	}
	// Wrap past midnight: [s, 24) ∪ [0, e).
	return hour >= s || hour < e
}

// lookup finds key in a JSON object sub-map (map[string]any with numeric
// values), case-insensitively. Returns the float value and whether found.
func lookup(sub any, key string) (float64, bool) {
	if key == "" {
		return 0, false
	}
	m, ok := sub.(map[string]any)
	if !ok {
		return 0, false
	}
	// Exact key first (common case), then case-insensitive scan.
	if raw, ok := m[key]; ok {
		if f, ok := toFloat(raw); ok {
			return f, true
		}
	}
	for k, raw := range m {
		if strings.EqualFold(k, key) {
			if f, ok := toFloat(raw); ok {
				return f, true
			}
		}
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

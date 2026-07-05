// Package floors resolves the effective per-request floor price for a
// placement from its base floor plus optional device/geo overrides stored
// in placements.floor_config (JSONB).
//
// Config shape (all keys optional):
//
//	{
//	  "device": {"mobile": 1.5, "desktop": 2.0},
//	  "geo":    {"USA": 2.5, "GBR": 1.8}
//	}
//
// Resolution is publisher-favouring: the effective floor is the MAX of the
// base floor and every override that matches the request (a request that is
// both mobile and in the USA takes the higher of the two). An empty/nil
// config returns the base floor unchanged, so existing placements are
// untouched.
package floors

import "strings"

// Effective returns the floor to advertise for a request on a placement.
// device is matched case-insensitively; geo is matched case-insensitively
// against the country-code keys.
func Effective(base float64, config map[string]any, device, geo string) float64 {
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
	return floor
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

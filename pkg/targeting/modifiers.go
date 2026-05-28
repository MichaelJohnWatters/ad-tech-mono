// Package targeting - bid modifiers.
//
// Modifiers adjust the base bid by percentage for specific dimensions.
// They stack multiplicatively: mobile(+20%) * UK(+15%) * peak_hours(+10%)
// = base * 1.20 * 1.15 * 1.10.
//
// Applied after targeting evaluation, before bid shading.
package targeting

// Modifiers holds bid adjustments per dimension.
type Modifiers struct {
	Device      map[string]float64 // "mobile": 20 (means +20%)
	GeoCountry  map[string]float64
	GeoRegion   map[string]float64
	TimeOfDay   []TimeModifier
	DayOfWeek   map[string]float64
	Audience    map[string]float64
	Inventory   map[string]float64 // "app": 10
	// Bounds (safety rails)
	MaxPositive float64 // max single modifier (default 200%)
	MaxNegative float64 // max single negative modifier (default -80%)
	MaxCombined float64 // max total combined modifier (default 300%)
}

// TimeModifier adjusts bid for a time window.
type TimeModifier struct {
	StartHour int     // 0-23
	EndHour   int     // 0-23
	Modifier  float64 // percentage (+10 = bid 10% higher)
}

// ModifierContext holds the signals used to look up modifiers.
type ModifierContext struct {
	Device      string
	GeoCountry  string
	GeoRegion   string
	HourOfDay   int
	DayOfWeek   string // mon, tue, wed, etc.
	Segments    []string
	Inventory   string // site, app
}

// ApplyModifiers calculates the modified bid price.
// Returns the adjusted bid and the total multiplier applied.
func ApplyModifiers(baseBid float64, mods Modifiers, ctx ModifierContext) (float64, float64) {
	if mods.MaxPositive == 0 {
		mods.MaxPositive = 200
	}
	if mods.MaxNegative == 0 {
		mods.MaxNegative = -80
	}
	if mods.MaxCombined == 0 {
		mods.MaxCombined = 300
	}

	multiplier := 1.0

	// Device modifier
	if v, ok := mods.Device[ctx.Device]; ok {
		multiplier *= clampModifier(v, mods.MaxPositive, mods.MaxNegative)
	}

	// Geo country
	if v, ok := mods.GeoCountry[ctx.GeoCountry]; ok {
		multiplier *= clampModifier(v, mods.MaxPositive, mods.MaxNegative)
	}

	// Geo region
	if v, ok := mods.GeoRegion[ctx.GeoRegion]; ok {
		multiplier *= clampModifier(v, mods.MaxPositive, mods.MaxNegative)
	}

	// Time of day
	for _, tm := range mods.TimeOfDay {
		if isInTimeWindow(ctx.HourOfDay, tm.StartHour, tm.EndHour) {
			multiplier *= clampModifier(tm.Modifier, mods.MaxPositive, mods.MaxNegative)
			break // first matching window
		}
	}

	// Day of week
	if v, ok := mods.DayOfWeek[ctx.DayOfWeek]; ok {
		multiplier *= clampModifier(v, mods.MaxPositive, mods.MaxNegative)
	}

	// Audience segments (take the highest matching modifier)
	bestAudienceMod := 0.0
	for _, seg := range ctx.Segments {
		if v, ok := mods.Audience[seg]; ok && v > bestAudienceMod {
			bestAudienceMod = v
		}
	}
	if bestAudienceMod != 0 {
		multiplier *= clampModifier(bestAudienceMod, mods.MaxPositive, mods.MaxNegative)
	}

	// Inventory type
	if v, ok := mods.Inventory[ctx.Inventory]; ok {
		multiplier *= clampModifier(v, mods.MaxPositive, mods.MaxNegative)
	}

	// Cap combined multiplier
	maxMultiplier := 1.0 + mods.MaxCombined/100.0
	minMultiplier := 1.0 + mods.MaxNegative/100.0 // e.g. 0.2 for -80%
	if multiplier > maxMultiplier {
		multiplier = maxMultiplier
	}
	if multiplier < minMultiplier {
		multiplier = minMultiplier
	}

	return baseBid * multiplier, multiplier
}

// clampModifier converts a percentage modifier to a multiplier and clamps it.
// +20% -> 1.20, -10% -> 0.90
func clampModifier(pct, maxPos, maxNeg float64) float64 {
	if pct > maxPos {
		pct = maxPos
	}
	if pct < maxNeg {
		pct = maxNeg
	}
	return 1.0 + pct/100.0
}

func isInTimeWindow(hour, start, end int) bool {
	if start <= end {
		return hour >= start && hour < end
	}
	// Wraps midnight: e.g. 22-06
	return hour >= start || hour < end
}

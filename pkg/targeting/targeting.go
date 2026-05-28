// Package targeting evaluates whether a bid request matches a line item's
// targeting rules. Supports inclusion and exclusion across all dimensions.
//
// Evaluation order:
//  1. Check inclusions: request must match at least one value in every included dimension
//  2. Check exclusions: request must NOT match any excluded value in any dimension
//
// Inclusion logic: AND across dimensions, OR within a dimension.
// Exclusion logic: OR across everything (any match = excluded).
//
// Usage:
//
//	match := targeting.Evaluate(rules, request)
//	if !match.Matched { /* no bid */ }
package targeting

// Rules defines the inclusion and exclusion targeting for a line item.
type Rules struct {
	Include TargetingSet
	Exclude TargetingSet
}

// TargetingSet holds targeting values per dimension.
type TargetingSet struct {
	Geo            []string          // country/region codes
	Device         []string          // mobile, desktop, tablet, ctv
	OS             []string          // iOS, Android, Windows
	Segments       []string          // audience segment IDs
	Domains        []string          // publisher domains
	AppBundles     []string          // app bundle IDs
	Categories     []string          // IAB content categories
	PlacementIDs   []string          // specific placement IDs
	Languages      []string          // language codes
	InventoryType  []string          // site, app
	Keywords       []string          // page keywords
	Custom         map[string]string // custom key-value pairs
}

// Request represents the targeting-relevant signals from a bid request.
type Request struct {
	Geo           string   // country or region code
	Device        string   // mobile, desktop, tablet, ctv
	OS            string   // iOS, Android
	Segments      []string // user's audience segments
	Domain        string   // publisher domain
	AppBundle     string   // app bundle ID
	Categories    []string // page IAB categories
	PlacementID   string   // placement ID
	Language      string   // content language
	InventoryType string   // site or app
	Keywords      []string // page keywords
}

// Result of a targeting evaluation.
type Result struct {
	Matched        bool
	FailedDimension string // which dimension caused the failure (if not matched)
	FailedReason   string // "no_inclusion_match" or "excluded"
}

// Evaluate checks if a request matches the targeting rules.
func Evaluate(rules Rules, req Request) Result {
	// Step 1: Check inclusions (AND across dimensions, OR within)
	if !checkInclusions(rules.Include, req) {
		return Result{Matched: false, FailedReason: "no_inclusion_match"}
	}

	// Step 2: Check exclusions (any match in any dimension = excluded)
	if dim := checkExclusions(rules.Exclude, req); dim != "" {
		return Result{Matched: false, FailedDimension: dim, FailedReason: "excluded"}
	}

	return Result{Matched: true}
}

// checkInclusions verifies the request matches all included dimensions.
// An empty inclusion list for a dimension means "match all" (no restriction).
func checkInclusions(include TargetingSet, req Request) bool {
	if len(include.Geo) > 0 && !containsOrPrefix(include.Geo, req.Geo) {
		return false
	}
	if len(include.Device) > 0 && !contains(include.Device, req.Device) {
		return false
	}
	if len(include.OS) > 0 && !contains(include.OS, req.OS) {
		return false
	}
	if len(include.Segments) > 0 && !containsAny(include.Segments, req.Segments) {
		return false
	}
	if len(include.Domains) > 0 && !contains(include.Domains, req.Domain) {
		return false
	}
	if len(include.AppBundles) > 0 && !contains(include.AppBundles, req.AppBundle) {
		return false
	}
	if len(include.Categories) > 0 && !containsAny(include.Categories, req.Categories) {
		return false
	}
	if len(include.PlacementIDs) > 0 && !contains(include.PlacementIDs, req.PlacementID) {
		return false
	}
	if len(include.Languages) > 0 && !contains(include.Languages, req.Language) {
		return false
	}
	if len(include.InventoryType) > 0 && !contains(include.InventoryType, req.InventoryType) {
		return false
	}
	if len(include.Keywords) > 0 && !containsAny(include.Keywords, req.Keywords) {
		return false
	}
	return true
}

// checkExclusions returns the dimension name that caused exclusion, or "" if none.
func checkExclusions(exclude TargetingSet, req Request) string {
	if len(exclude.Geo) > 0 && containsOrPrefix(exclude.Geo, req.Geo) {
		return "geo"
	}
	if len(exclude.Device) > 0 && contains(exclude.Device, req.Device) {
		return "device"
	}
	if len(exclude.OS) > 0 && contains(exclude.OS, req.OS) {
		return "os"
	}
	if len(exclude.Segments) > 0 && containsAny(exclude.Segments, req.Segments) {
		return "segments"
	}
	if len(exclude.Domains) > 0 && contains(exclude.Domains, req.Domain) {
		return "domains"
	}
	if len(exclude.AppBundles) > 0 && contains(exclude.AppBundles, req.AppBundle) {
		return "app_bundles"
	}
	if len(exclude.Categories) > 0 && containsAny(exclude.Categories, req.Categories) {
		return "categories"
	}
	if len(exclude.PlacementIDs) > 0 && contains(exclude.PlacementIDs, req.PlacementID) {
		return "placement_ids"
	}
	if len(exclude.Keywords) > 0 && containsAny(exclude.Keywords, req.Keywords) {
		return "keywords"
	}
	return ""
}

// contains checks if a slice contains a value (exact match).
func contains(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}

// containsAny checks if any value in vals exists in slice.
func containsAny(slice []string, vals []string) bool {
	set := make(map[string]bool, len(slice))
	for _, s := range slice {
		set[s] = true
	}
	for _, v := range vals {
		if set[v] {
			return true
		}
	}
	return false
}

// containsOrPrefix checks if val matches any entry in slice,
// supporting geo hierarchy (e.g. "UK" matches "UK_london").
func containsOrPrefix(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
		// Check if val starts with the entry (geo hierarchy)
		// e.g. include "UK" matches request geo "UK_london"
		if len(val) > len(s) && val[:len(s)] == s && val[len(s)] == '_' {
			return true
		}
		// Check if entry starts with val (e.g. include "UK_london" matches "UK_london")
		if len(s) > len(val) && s[:len(val)] == val && s[len(val)] == '_' {
			return true
		}
	}
	return false
}

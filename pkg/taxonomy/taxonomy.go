// Package taxonomy is the platform's authoritative IAB Content Taxonomy.
//
// The platform runs on IAB Content Taxonomy 1.0 codes — the "IABxx" string
// scheme (OpenRTB category taxonomy `cattax` = 1), with tier-2 subcategories
// written as "IAB17-1". This package holds the complete tier-1 set (IAB1–IAB26)
// plus lookup, validation, and tier-2→tier-1 resolution so the rest of the
// codebase can validate/normalise category codes instead of hand-maintaining
// partial lists.
//
// Why 1.0 and not 3.0: every category the platform stores (Site.Cat, creative
// categories, seed data, the contextual classifier) is an IABxx code. Content
// Taxonomy 2.0/3.0 use unrelated numeric ids and would require migrating all of
// that data — out of scope. `cattax` is available on OpenRTB objects for
// callers that need to declare which taxonomy a code belongs to.
package taxonomy

import (
	"sort"
	"strings"
)

// CatTax1 is the OpenRTB `cattax` value for IAB Content Taxonomy 1.0 — the
// scheme this package covers. Provided so callers stamping category taxonomies
// don't hardcode the magic number.
const CatTax1 = 1

// tier1Names is the complete IAB Content Taxonomy 1.0 tier-1 category set.
// Codes and canonical names per the IAB spec.
var tier1Names = map[string]string{
	"IAB1":  "Arts & Entertainment",
	"IAB2":  "Automotive",
	"IAB3":  "Business",
	"IAB4":  "Careers",
	"IAB5":  "Education",
	"IAB6":  "Family & Parenting",
	"IAB7":  "Health & Fitness",
	"IAB8":  "Food & Drink",
	"IAB9":  "Hobbies & Interests",
	"IAB10": "Home & Garden",
	"IAB11": "Law, Government & Politics",
	"IAB12": "News",
	"IAB13": "Personal Finance",
	"IAB14": "Society",
	"IAB15": "Science",
	"IAB16": "Pets",
	"IAB17": "Sports",
	"IAB18": "Style & Fashion",
	"IAB19": "Technology & Computing",
	"IAB20": "Travel",
	"IAB21": "Real Estate",
	"IAB22": "Shopping",
	"IAB23": "Religion & Spirituality",
	"IAB24": "Uncategorized",
	"IAB25": "Non-Standard Content",
	"IAB26": "Illegal Content",
}

// Category is a tier-1 IAB category: its code and canonical name.
type Category struct {
	Code string
	Name string
}

// Tier1 returns the tier-1 code for any IAB code — "IAB17-1" → "IAB17",
// "IAB17" → "IAB17". Whitespace is trimmed; the result is not validated (use
// IsValid for that).
func Tier1(code string) string {
	code = strings.TrimSpace(code)
	if i := strings.IndexByte(code, '-'); i >= 0 {
		return code[:i]
	}
	return code
}

// IsValid reports whether a code's tier-1 portion is a known IAB Content
// Taxonomy 1.0 category. Tier-2 subcodes ("IAB17-1") are considered valid when
// their tier-1 parent is known — the tier-2 number itself isn't enumerated.
func IsValid(code string) bool {
	_, ok := tier1Names[Tier1(code)]
	return ok
}

// Name returns the canonical tier-1 category name for a code (tier-1 or
// tier-2). The bool is false for an unknown code.
func Name(code string) (string, bool) {
	n, ok := tier1Names[Tier1(code)]
	return n, ok
}

// WithParents returns cats with every tier-2 code's tier-1 parent guaranteed to
// be present, order-preserving and de-duplicated. E.g. ["IAB17-1"] →
// ["IAB17-1", "IAB17"]. Unknown codes are passed through untouched so callers
// don't silently lose data. Used to normalise publisher-declared categories so
// a subcategory always implies its parent for targeting.
func WithParents(cats []string) []string {
	if len(cats) == 0 {
		return cats
	}
	seen := make(map[string]bool, len(cats)*2)
	out := make([]string, 0, len(cats)*2)
	add := func(c string) {
		if c == "" || seen[c] {
			return
		}
		seen[c] = true
		out = append(out, c)
	}
	for _, c := range cats {
		c = strings.TrimSpace(c)
		add(c)
		if parent := Tier1(c); parent != c && IsValid(parent) {
			add(parent)
		}
	}
	return out
}

// All returns every tier-1 category, sorted by numeric code (IAB1..IAB26).
func All() []Category {
	out := make([]Category, 0, len(tier1Names))
	for code, name := range tier1Names {
		out = append(out, Category{Code: code, Name: name})
	}
	sort.Slice(out, func(i, j int) bool {
		return codeNum(out[i].Code) < codeNum(out[j].Code)
	})
	return out
}

// codeNum extracts the integer from a tier-1 "IABn" code for ordering; unknown
// shapes sort last.
func codeNum(code string) int {
	n := 0
	digits := strings.TrimPrefix(code, "IAB")
	if digits == code { // no prefix
		return 1 << 30
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 1 << 30
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// Package glossary is a static, code-grounded ad-tech reference: plain-English
// definitions of the industry terms this platform touches, each paired with a
// "How this platform does it" note pointing at the concrete service, package,
// or file that implements the concept. It is a learning / interview study aid,
// not a data model — the terms live in Go source (no DB, no migration), read
// once and served read-only to the staff portal.
//
// Every Term carries a Name, a Definition (2-4 plain-English sentences), a
// Category (grouping), and a HowWeDoIt note. When a term is an industry concept
// this platform does NOT implement, HowWeDoIt says so plainly rather than
// inventing a claim.
package glossary

import "sort"

// Category groups related terms. Kept as typed constants so the section order
// below and any consumer stay in sync.
type Category string

const (
	CatMarketplace  Category = "Marketplace roles"
	CatAuctions     Category = "Auctions & pricing"
	CatDeals        Category = "Deals"
	CatBuyingModels Category = "Buying models"
	CatDelivery     Category = "Budget & delivery"
	CatCreative     Category = "Creative & formats"
	CatIdentity     Category = "Identity & privacy"
	CatSupplyChain  Category = "Supply chain & fraud"
	CatMeasurement  Category = "Measurement & data"
	CatAudience     Category = "Segments & audience"
)

// categoryOrder is the display order for the grouped view — roughly the path a
// bid request takes (roles → auction → deal → money → creative → identity →
// supply-chain safety → measurement → audience data).
var categoryOrder = []Category{
	CatMarketplace,
	CatAuctions,
	CatDeals,
	CatBuyingModels,
	CatDelivery,
	CatCreative,
	CatIdentity,
	CatSupplyChain,
	CatMeasurement,
	CatAudience,
}

// Term is one glossary entry.
type Term struct {
	// Name is the term itself (e.g. "SSP", "Bid shading").
	Name string `json:"name"`
	// Definition is a crisp, plain-English explanation (2-4 sentences).
	Definition string `json:"definition"`
	// Category groups the term (see the Cat* constants).
	Category Category `json:"category"`
	// HowWeDoIt ties the term to THIS codebase — a service/package/file pointer,
	// or an explicit "not implemented" when the platform doesn't do it.
	HowWeDoIt string `json:"how_we_do_it"`
}

// Group is one category with its terms, for the grouped/rendered view.
type Group struct {
	Category Category `json:"category"`
	Terms    []Term   `json:"terms"`
}

// All returns every term in a stable order (by category order, then name).
func All() []Term {
	out := make([]Term, len(terms))
	copy(out, terms)
	rank := map[Category]int{}
	for i, c := range categoryOrder {
		rank[c] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Category] != rank[out[j].Category] {
			return rank[out[i].Category] < rank[out[j].Category]
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Grouped returns the terms bucketed by category, in categoryOrder, with each
// category's terms sorted by name. Empty categories are omitted.
func Grouped() []Group {
	byCat := map[Category][]Term{}
	for _, t := range terms {
		byCat[t.Category] = append(byCat[t.Category], t)
	}
	var groups []Group
	for _, c := range categoryOrder {
		ts := byCat[c]
		if len(ts) == 0 {
			continue
		}
		sort.SliceStable(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
		groups = append(groups, Group{Category: c, Terms: ts})
	}
	return groups
}

// Count returns the number of terms defined.
func Count() int { return len(terms) }

// Categories returns the category display order.
func Categories() []Category { return append([]Category(nil), categoryOrder...) }

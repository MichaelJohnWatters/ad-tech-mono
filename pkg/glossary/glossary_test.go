package glossary

import (
	"strings"
	"testing"
)

// TestTermsWellFormed asserts every term has a name, a definition, a valid
// category, and a "how we do it" note — no empties. This is the invariant that
// keeps the glossary honest: an entry with a blank field would render as a
// broken card in the portal.
func TestTermsWellFormed(t *testing.T) {
	if Count() == 0 {
		t.Fatal("glossary is empty")
	}
	valid := map[Category]bool{}
	for _, c := range Categories() {
		valid[c] = true
	}
	seen := map[string]bool{}
	for i, term := range All() {
		if strings.TrimSpace(term.Name) == "" {
			t.Errorf("term %d has an empty Name", i)
		}
		if strings.TrimSpace(term.Definition) == "" {
			t.Errorf("term %q has an empty Definition", term.Name)
		}
		if strings.TrimSpace(term.HowWeDoIt) == "" {
			t.Errorf("term %q has an empty HowWeDoIt note", term.Name)
		}
		if !valid[term.Category] {
			t.Errorf("term %q has unknown category %q", term.Name, term.Category)
		}
		if seen[term.Name] {
			t.Errorf("duplicate term name %q", term.Name)
		}
		seen[term.Name] = true
	}
}

// TestGroupedCoversEveryTerm asserts Grouped() partitions all terms exactly once
// and only emits non-empty groups in the declared category order.
func TestGroupedCoversEveryTerm(t *testing.T) {
	groups := Grouped()
	if len(groups) == 0 {
		t.Fatal("Grouped() returned no groups")
	}
	total := 0
	rank := map[Category]int{}
	for i, c := range Categories() {
		rank[c] = i
	}
	last := -1
	for _, g := range groups {
		if len(g.Terms) == 0 {
			t.Errorf("group %q is empty (should be omitted)", g.Category)
		}
		r, ok := rank[g.Category]
		if !ok {
			t.Errorf("group has unknown category %q", g.Category)
		}
		if r <= last {
			t.Errorf("groups out of order: %q (rank %d) after rank %d", g.Category, r, last)
		}
		last = r
		total += len(g.Terms)
		// terms sorted within group
		for i := 1; i < len(g.Terms); i++ {
			if g.Terms[i-1].Name > g.Terms[i].Name {
				t.Errorf("group %q not sorted: %q before %q", g.Category, g.Terms[i-1].Name, g.Terms[i].Name)
			}
		}
	}
	if total != Count() {
		t.Errorf("Grouped() covers %d terms, want %d", total, Count())
	}
}

// TestComprehensive is a light guard that the set stays a real reference (not a
// stub) and spans the breadth of categories.
func TestComprehensive(t *testing.T) {
	if Count() < 50 {
		t.Errorf("glossary has only %d terms; expected a comprehensive set (~50+)", Count())
	}
	if len(Grouped()) < 8 {
		t.Errorf("glossary spans only %d categories; expected broad coverage", len(Grouped()))
	}
}

package postgres

import (
	"sort"
	"testing"
	"unsafe"
)

// dup forces a fresh string allocation so test inputs don't accidentally share
// backing bytes (the way distinct sql.Scan calls wouldn't).
func dup(s string) string { return string([]byte(s)) }

func neighbourIDs(links []IdentityLink) []string {
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.ID
	}
	sort.Strings(out)
	return out
}

func eqStrings(got, want []string) bool {
	sort.Strings(want)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestAdjacencyBuilder(t *testing.T) {
	b := newAdjacencyBuilder()
	b.add(dup("a"), dup("b"), 1.0)
	b.add(dup("b"), dup("c"), 0.6)
	b.add(dup("a"), dup("a"), 1.0) // self-link ignored
	b.add(dup(""), dup("x"), 1.0)  // empty ignored
	b.add(dup("a"), dup("b"), 0.3) // duplicate pair, lower conf → keep max (1.0)
	adj := b.build()

	t.Run("bidirectional, self/empty skipped", func(t *testing.T) {
		want := map[string][]string{"a": {"b"}, "b": {"a", "c"}, "c": {"b"}}
		for id, w := range want {
			if !eqStrings(neighbourIDs(adj[id]), w) {
				t.Errorf("adj[%q] = %v, want %v", id, neighbourIDs(adj[id]), w)
			}
		}
		if _, ok := adj["x"]; ok {
			t.Error("empty-source edge should have been skipped")
		}
	})

	t.Run("keeps the strongest confidence per pair", func(t *testing.T) {
		var ab float64
		for _, l := range adj["a"] {
			if l.ID == "b" {
				ab = l.Confidence
			}
		}
		if ab != 1.0 {
			t.Errorf("confidence a→b = %v, want 1.0 (max of 1.0 and 0.3)", ab)
		}
	})

	t.Run("ids are interned (shared backing)", func(t *testing.T) {
		var keyB, sliceB string
		for k := range adj {
			if k == "b" {
				keyB = k
			}
		}
		for _, l := range adj["a"] {
			if l.ID == "b" {
				sliceB = l.ID
			}
		}
		if keyB == "" || sliceB == "" {
			t.Fatal("could not locate id \"b\" in both positions")
		}
		if unsafe.StringData(keyB) != unsafe.StringData(sliceB) {
			t.Error("id \"b\" not interned: map key and slice entry have different backing bytes")
		}
	})
}

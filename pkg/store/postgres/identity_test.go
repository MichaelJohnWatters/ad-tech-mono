package postgres

import (
	"sort"
	"testing"
	"unsafe"
)

// dup forces a fresh string allocation so test inputs don't accidentally share
// backing bytes (the way distinct sql.Scan calls wouldn't).
func dup(s string) string { return string([]byte(s)) }

func sortedEqual(got, want []string) bool {
	g := append([]string(nil), got...)
	sort.Strings(g)
	sort.Strings(want)
	if len(g) != len(want) {
		return false
	}
	for i := range g {
		if g[i] != want[i] {
			return false
		}
	}
	return true
}

func TestAdjacencyBuilder(t *testing.T) {
	b := newAdjacencyBuilder()
	b.add(dup("a"), dup("b"))
	b.add(dup("b"), dup("c"))
	b.add(dup("a"), dup("a")) // self-link ignored
	b.add(dup(""), dup("x"))  // empty ignored
	adj := b.build()

	t.Run("bidirectional, self/empty skipped", func(t *testing.T) {
		want := map[string][]string{"a": {"b"}, "b": {"a", "c"}, "c": {"b"}}
		for id, w := range want {
			if !sortedEqual(adj[id], w) {
				t.Errorf("adj[%q] = %v, want %v", id, adj[id], w)
			}
		}
		if _, ok := adj["x"]; ok {
			t.Error("empty-source edge should have been skipped")
		}
		if _, ok := adj[""]; ok {
			t.Error("empty key present")
		}
	})

	t.Run("ids are interned (shared backing)", func(t *testing.T) {
		// "b" appears as a map key and inside adj["a"]; interning means both
		// point at the same backing bytes — one allocation per unique id.
		var keyB, sliceB string
		for k := range adj {
			if k == "b" {
				keyB = k
			}
		}
		for _, v := range adj["a"] {
			if v == "b" {
				sliceB = v
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

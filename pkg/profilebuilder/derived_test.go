package profilebuilder

import (
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
)

func set(persons ...string) map[string]bool {
	out := map[string]bool{}
	for _, p := range persons {
		out[p] = true
	}
	return out
}

func TestCompositeQualified(t *testing.T) {
	a := set("p1", "p2", "p3")
	b := set("p2", "p3", "p4")
	c := set("p3", "p5")

	t.Run("all_of intersects", func(t *testing.T) {
		got := compositeQualified([]map[string]bool{a, b}, nil, nil)
		if len(got) != 2 || got[0] != "p2" || got[1] != "p3" {
			t.Errorf("got %v, want [p2 p3]", got)
		}
	})
	t.Run("any_of unions", func(t *testing.T) {
		got := compositeQualified(nil, []map[string]bool{a, c}, nil)
		if len(got) != 4 {
			t.Errorf("got %v, want 4 persons (union of a+c)", got)
		}
	})
	t.Run("none_of excludes", func(t *testing.T) {
		got := compositeQualified([]map[string]bool{a}, nil, []map[string]bool{c})
		if len(got) != 2 || got[0] != "p1" || got[1] != "p2" {
			t.Errorf("got %v, want [p1 p2] (p3 excluded by none_of)", got)
		}
	})
	t.Run("all_of AND any_of", func(t *testing.T) {
		got := compositeQualified([]map[string]bool{a}, []map[string]bool{c}, nil)
		if len(got) != 1 || got[0] != "p3" {
			t.Errorf("got %v, want [p3] (in a AND in c)", got)
		}
	})
}

func TestLookalikeQualified(t *testing.T) {
	rule := LookalikeRule{SeedSegment: "seed", TopCategories: 10, MinSimilarity: 0.5, MaxMembers: 100}
	seed := set("s1", "s2")
	byPerson := map[string]map[string]bool{
		"s1":       set("sports", "news"),
		"s2":       set("sports", "cars"),
		"similar":  set("sports", "news"),  // shares 2/3 seed cats
		"partial":  set("cars"),            // 1/3 < 0.5
		"stranger": set("finance", "food"), // 0/3
	}
	got := lookalikeQualified(seed, byPerson, rule)
	if len(got) != 1 || got[0] != "similar" {
		t.Fatalf("got %v, want [similar]", got)
	}

	t.Run("seed persons never self-enroll", func(t *testing.T) {
		for _, p := range got {
			if seed[p] {
				t.Errorf("seed person %s enrolled in its own lookalike", p)
			}
		}
	})
	t.Run("max_members caps", func(t *testing.T) {
		capped := rule
		capped.MinSimilarity = 0.1
		capped.MaxMembers = 1
		got := lookalikeQualified(seed, byPerson, capped)
		if len(got) != 1 {
			t.Errorf("cap ignored: got %v", got)
		}
		if got[0] != "similar" {
			t.Errorf("cap kept %v, want the highest-scoring (similar)", got)
		}
	})
	t.Run("behaviour-less seed matches nobody", func(t *testing.T) {
		if got := lookalikeQualified(set("ghost"), byPerson, rule); len(got) != 0 {
			t.Errorf("got %v, want none (seed has no categories)", got)
		}
	})
}

func TestPersonCategories(t *testing.T) {
	c := Clusters{
		Members:  map[string][]string{"person:a": {"a", "a2"}},
		PersonOf: map[string]string{"a": "person:a", "a2": "person:a"},
	}
	rows := []datalake.Record{
		{"user_id": "a", "categories": "Sports, News"},
		{"user_id": "a2", "categories": "cars"},
		{"user_id": "", "household_id": "hh:x", "categories": "food"},
	}
	got := personCategories(rows, c)
	if !got["person:a"]["sports"] || !got["person:a"]["news"] || !got["person:a"]["cars"] {
		t.Errorf("cluster members' categories not merged at person level: %v", got["person:a"])
	}
	if !got["hh:x"]["food"] {
		t.Errorf("household fallback key missing: %v", got)
	}
}

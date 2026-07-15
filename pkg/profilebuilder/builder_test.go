package profilebuilder

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

func adjacency(edges ...[3]any) map[string][]postgres.IdentityLink {
	adj := map[string][]postgres.IdentityLink{}
	add := func(a, b string, conf float64) {
		adj[a] = append(adj[a], postgres.IdentityLink{ID: b, Confidence: conf})
	}
	for _, e := range edges {
		a, b, conf := e[0].(string), e[1].(string), e[2].(float64)
		add(a, b, conf)
		add(b, a, conf)
	}
	return adj
}

func TestBuildClusters(t *testing.T) {
	t.Run("connected components with confidence gate", func(t *testing.T) {
		adj := adjacency(
			[3]any{"u1", "em:a", 1.0},
			[3]any{"em:a", "dev1", 1.0},
			[3]any{"u2", "dev2", 1.0},   // separate cluster
			[3]any{"u1", "u2", 0.3},     // weak probabilistic edge — must NOT merge
			[3]any{"solo", "solo2", 1.0},
		)
		c, dropped := buildClusters(adj, 0.5, 100)
		if dropped != 0 {
			t.Fatalf("dropped = %d, want 0", dropped)
		}
		if len(c.Members) != 3 {
			t.Fatalf("clusters = %d, want 3 (u1-cluster, u2-cluster, solo-cluster)", len(c.Members))
		}
		if c.PersonOf["u1"] == c.PersonOf["u2"] {
			t.Error("weak edge merged u1 and u2 — confidence gate broken")
		}
		if c.PersonOf["u1"] != c.PersonOf["dev1"] {
			t.Error("u1 and dev1 should share a person via em:a")
		}
	})

	t.Run("household edges never merge people", func(t *testing.T) {
		adj := adjacency(
			[3]any{"alice", "hh:home", 1.0},
			[3]any{"bob", "hh:home", 1.0},
		)
		c, _ := buildClusters(adj, 0.5, 100)
		if len(c.Members) != 0 {
			t.Fatalf("household-only links created %d clusters, want 0 (roommates are not one person)", len(c.Members))
		}
	})

	t.Run("mega-cluster guard", func(t *testing.T) {
		var edges [][3]any
		for i := 0; i < 10; i++ {
			edges = append(edges, [3]any{"hub", "spoke-" + string(rune('a'+i)), 1.0})
		}
		c, dropped := buildClusters(adjacency(edges...), 0.5, 5)
		if dropped != 1 {
			t.Errorf("dropped = %d, want 1", dropped)
		}
		if len(c.Members) != 0 {
			t.Errorf("oversized cluster materialized anyway (%d clusters)", len(c.Members))
		}
	})

	t.Run("person id is stable and deterministic", func(t *testing.T) {
		adj := adjacency([3]any{"b-id", "a-id", 1.0})
		c, _ := buildClusters(adj, 0.5, 100)
		if len(c.Members) != 1 {
			t.Fatalf("clusters = %d, want 1", len(c.Members))
		}
		if _, ok := c.Members["person:a-id"]; !ok {
			t.Errorf("person id should derive from smallest member; got %v", c.Members)
		}
	})
}

func TestEvaluateRule(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	row := func(kind, user, categories string, age time.Duration) datalake.Record {
		return datalake.Record{
			"kind": kind, "user_id": user, "categories": categories,
			"channel": "display", "observed_at": now.Add(-age),
		}
	}
	rows := []datalake.Record{
		row("request", "u1", "sports,news", time.Hour),
		row("request", "u1", "sports", 2*time.Hour),
		row("request", "u1", "finance", 3*time.Hour),
		row("request", "u2", "sports", time.Hour),
		row("request", "u3", "sports", 45*24*time.Hour), // outside window
		row("click", "u4", "sports", time.Hour),
	}

	t.Run("min_count with category filter", func(t *testing.T) {
		rule := Rule{Event: "request", Category: "sports", MinCount: 2, WindowDays: 30}
		got := evaluateRule(rows, rule, now)
		if len(got) != 1 || got[0] != "u1" {
			t.Errorf("got %v, want [u1] (u2 has 1 < 2, u3 outside window)", got)
		}
	})
	t.Run("event kind filter", func(t *testing.T) {
		rule := Rule{Event: "click", MinCount: 1, WindowDays: 30}
		got := evaluateRule(rows, rule, now)
		if len(got) != 1 || got[0] != "u4" {
			t.Errorf("got %v, want [u4]", got)
		}
	})
	t.Run("household fallback key", func(t *testing.T) {
		hh := []datalake.Record{{
			"kind": "request", "user_id": "", "household_id": "hh:x",
			"categories": "", "observed_at": now,
		}}
		got := evaluateRule(hh, Rule{Event: "request", MinCount: 1, WindowDays: 30}, now)
		if len(got) != 1 || got[0] != "hh:x" {
			t.Errorf("got %v, want [hh:x]", got)
		}
	})
}

func TestExpandKeys(t *testing.T) {
	c := Clusters{
		Members:  map[string][]string{"person:a": {"a", "b", "c"}},
		PersonOf: map[string]string{"a": "person:a", "b": "person:a", "c": "person:a"},
	}
	got := expandKeys(c, []string{"b", "outsider"})
	want := map[string]bool{"a": true, "b": true, "c": true, "outsider": true}
	if len(got) != len(want) {
		t.Fatalf("expanded = %v, want keys of %v", got, want)
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("unexpected expansion member %q", k)
		}
	}
}

func TestParseRule(t *testing.T) {
	r, err := ParseRule([]byte(`{"event":"request","category":"sports"}`))
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	if r.MinCount != 1 || r.WindowDays != 30 {
		t.Errorf("defaults not applied: %+v", r)
	}
	if _, err := ParseRule([]byte(`{"category":"sports"}`)); err == nil {
		t.Error("rule without event should fail")
	}
}

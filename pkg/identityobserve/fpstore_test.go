package identityobserve

import (
	"testing"
	"time"
)

func TestNormalizeUA(t *testing.T) {
	// Minor-version churn collapses to the same normalized UA.
	a := NormalizeUA("Mozilla/5.0 (Windows NT 10.0) Chrome/120.0.0.0 Safari/537.36")
	b := NormalizeUA("Mozilla/5.0 (Windows NT 10.0) Chrome/121.0.6167.85 Safari/537.36")
	if a != b {
		t.Errorf("minor-version UAs should normalize equal:\n%q\n%q", a, b)
	}
	if a == "" {
		t.Error("normalized UA is empty (over-stripped)")
	}
	// A genuinely different browser/OS must NOT collapse.
	c := NormalizeUA("Mozilla/5.0 (Macintosh) Firefox/120.0")
	if a == c {
		t.Errorf("different browser/OS should differ: %q == %q", a, c)
	}
}

func TestMemFPStore(t *testing.T) {
	s := newMemFPStore(1000)
	if got := s.Observe("fp", "u1", 3); len(got) != 0 {
		t.Errorf("first id: got %v, want none to link", got)
	}
	if got := s.Observe("fp", "u2", 3); len(got) != 1 || got[0] != "u1" {
		t.Errorf("second id: got %v, want [u1]", got)
	}
	if got := s.Observe("fp", "u2", 3); got != nil {
		t.Errorf("repeat id: got %v, want nil", got)
	}
	// Cap: at maxUsers=3, u1+u2+u3 fill it; u4 is skipped (returns nil).
	_ = s.Observe("fp", "u3", 3)
	if got := s.Observe("fp", "u4", 3); got != nil {
		t.Errorf("over cap: got %v, want nil (shared IP)", got)
	}
}

func TestFuzzyGroupsMinorVersions(t *testing.T) {
	fw := &fakeWriter{}
	o := New(fw, Config{Flush: time.Hour, ProbEnabled: true, ProbConf: 0.5, FPMaxUsers: 5, FuzzyUA: true}, quietLog())
	o.Start()
	// Same IP, UA differing only by version → fuzzy normalization groups them.
	o.Observe([]Signal{sig("userA", "x")}, "1.2.3.4|Chrome/120.0.0.0")
	o.Observe([]Signal{sig("userB", "x")}, "1.2.3.4|Chrome/121.0.6167.85")
	o.Stop()
	if len(fw.edges) != 1 {
		t.Fatalf("wrote %d edges, want 1 (fuzzy should link across versions): %+v", len(fw.edges), fw.edges)
	}
	if pairOnly(fw.edges[0]) != "userA|userB" {
		t.Errorf("wrong pair: %+v", fw.edges[0])
	}
}

func TestExactDoesNotGroupVersions(t *testing.T) {
	fw := &fakeWriter{}
	o := New(fw, Config{Flush: time.Hour, ProbEnabled: true, ProbConf: 0.5, FPMaxUsers: 5, FuzzyUA: false}, quietLog())
	o.Start()
	o.Observe([]Signal{sig("userA", "x")}, "1.2.3.4|Chrome/120.0.0.0")
	o.Observe([]Signal{sig("userB", "x")}, "1.2.3.4|Chrome/121.0.6167.85")
	o.Stop()
	if len(fw.edges) != 0 {
		t.Errorf("exact matching should NOT link across UA versions, got %+v", fw.edges)
	}
}

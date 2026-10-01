package main

import (
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/vmap"
)

func TestSelectBreaks(t *testing.T) {
	byName := map[string]vmap.BreakSpec{
		"pre":  {BreakID: "pre-roll"},
		"mid":  {BreakID: "mid-roll-1"},
		"post": {BreakID: "post-roll"},
	}
	ids := func(specs []vmap.BreakSpec) []string {
		out := make([]string, len(specs))
		for i, s := range specs {
			out[i] = s.BreakID
		}
		return out
	}
	eq := func(got, want []string) bool {
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

	cases := []struct {
		csv  string
		want []string
	}{
		{"pre", []string{"pre-roll"}},
		{"pre,mid", []string{"pre-roll", "mid-roll-1"}},
		{"pre,mid,post", []string{"pre-roll", "mid-roll-1", "post-roll"}},
		{"post,pre", []string{"pre-roll", "post-roll"}},            // canonical order preserved
		{"", []string{"pre-roll", "mid-roll-1", "post-roll"}},      // default = all
		{"bogus", []string{"pre-roll", "mid-roll-1", "post-roll"}}, // unknown → all
		{" PRE , MID ", []string{"pre-roll", "mid-roll-1"}},        // trim + case-insensitive
	}
	for _, c := range cases {
		got := ids(selectBreaks(c.csv, byName))
		if !eq(got, c.want) {
			t.Errorf("selectBreaks(%q) = %v, want %v", c.csv, got, c.want)
		}
	}
}

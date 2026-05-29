package targeting

import (
	"testing"
)

func TestClassifier_URLPatterns(t *testing.T) {
	c := NewClassifier()

	tests := []struct {
		url      string
		wantCats []string
	}{
		{"https://news.com/sport/football/match-report", []string{"IAB17", "IAB17-1"}},
		{"https://example.com/tech/ai-revolution", []string{"IAB19"}},
		{"https://example.com/finance/stock-market", []string{"IAB13"}},
		{"https://example.com/travel/holiday-deals", []string{"IAB20"}},
		{"https://example.com/unknown/page", nil},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			cats := c.Classify("", tt.url, nil, nil)
			if tt.wantCats == nil && cats != nil {
				t.Errorf("expected nil, got %v", cats)
			}
			if tt.wantCats != nil && len(cats) == 0 {
				t.Errorf("expected %v, got nil", tt.wantCats)
			}
		})
	}
}

func TestClassifier_DeclaredPriority(t *testing.T) {
	c := NewClassifier()

	// Publisher-declared should take priority over URL pattern
	cats := c.Classify("", "https://sports.com/football", []string{"IAB1"}, nil)
	if len(cats) != 1 || cats[0] != "IAB1" {
		t.Errorf("expected declared [IAB1], got %v", cats)
	}
}

func TestClassifier_KeywordFallback(t *testing.T) {
	c := NewClassifier()

	cats := c.Classify("", "https://example.com/page123", nil, []string{"bitcoin", "trading", "crypto"})
	found := false
	for _, cat := range cats {
		if cat == "IAB13" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected IAB13 from finance keywords, got %v", cats)
	}
}

func TestMatchKeywords(t *testing.T) {
	tests := []struct {
		name     string
		page     []string
		include  []string
		exclude  []string
		expected bool
	}{
		{"no targeting", []string{"sport"}, nil, nil, true},
		{"include match", []string{"football", "match"}, []string{"football"}, nil, true},
		{"include no match", []string{"cooking", "recipe"}, []string{"football"}, nil, false},
		{"exclude match", []string{"football", "accident"}, []string{"football"}, []string{"accident"}, false},
		{"exclude no match", []string{"football", "match"}, []string{"football"}, []string{"accident"}, true},
		{"stemming match", []string{"running"}, []string{"running"}, nil, true}, // exact match
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchKeywords(tt.page, tt.include, tt.exclude)
			if got != tt.expected {
				t.Errorf("got %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestStem(t *testing.T) {
	tests := map[string]string{
		"charging": "charg",
		"running":  "runn",
		"players":  "play",  // "ers" strips first
		"played":   "play",
		"quickly":  "quick",
		"cat":      "cat", // too short to stem
	}

	for input, want := range tests {
		got := stem(input)
		if got != want {
			t.Errorf("stem(%q) = %q, want %q", input, got, want)
		}
	}
}

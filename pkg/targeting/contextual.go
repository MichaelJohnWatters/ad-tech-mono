package targeting

import (
	"regexp"
	"strings"
)

// URLClassification maps URL patterns to IAB categories.
type URLClassification struct {
	Pattern    *regexp.Regexp
	Categories []string
}

// Classifier assigns IAB categories to pages based on URL patterns,
// publisher-declared categories, and keyword matching.
type Classifier struct {
	urlRules []URLClassification
}

// NewClassifier creates a contextual classifier with default URL rules.
func NewClassifier() *Classifier {
	return &Classifier{
		urlRules: defaultURLRules(),
	}
}

// Classify returns IAB categories for a page based on all available signals.
// Priority: publisher-declared > URL pattern > keywords.
func (c *Classifier) Classify(domain, pageURL string, declaredCategories, keywords []string) []string {
	// Publisher-declared categories take priority
	if len(declaredCategories) > 0 {
		return declaredCategories
	}

	// Try URL pattern matching
	cats := c.classifyURL(pageURL)
	if len(cats) > 0 {
		return cats
	}

	// Fall back to domain-level classification
	cats = c.classifyURL(domain)
	if len(cats) > 0 {
		return cats
	}

	// Fall back to keyword matching
	if len(keywords) > 0 {
		return classifyKeywords(keywords)
	}

	return nil
}

func (c *Classifier) classifyURL(url string) []string {
	lower := strings.ToLower(url)
	for _, rule := range c.urlRules {
		if rule.Pattern.MatchString(lower) {
			return rule.Categories
		}
	}
	return nil
}

// MatchKeywords checks if any of the page keywords match the campaign's
// keyword targeting (include/exclude). Returns true if matched.
func MatchKeywords(pageKeywords []string, includeKeywords, excludeKeywords []string) bool {
	if len(includeKeywords) == 0 && len(excludeKeywords) == 0 {
		return true // no keyword targeting = match all
	}

	pageSet := make(map[string]bool)
	for _, kw := range pageKeywords {
		// Normalise: lowercase, trim
		normalised := strings.ToLower(strings.TrimSpace(kw))
		pageSet[normalised] = true
		// Also add stemmed version (simple suffix stripping)
		pageSet[stem(normalised)] = true
	}

	// Check excludes first
	for _, ex := range excludeKeywords {
		normalised := strings.ToLower(strings.TrimSpace(ex))
		if pageSet[normalised] || pageSet[stem(normalised)] {
			return false // excluded keyword found
		}
	}

	// Check includes (at least one must match)
	if len(includeKeywords) == 0 {
		return true
	}
	for _, inc := range includeKeywords {
		normalised := strings.ToLower(strings.TrimSpace(inc))
		if pageSet[normalised] || pageSet[stem(normalised)] {
			return true
		}
	}
	return false
}

// stem does basic English suffix stripping for keyword matching.
func stem(word string) string {
	suffixes := []string{"ing", "tion", "sion", "ment", "ness", "ous", "ive", "able", "ible", "ers", "ed", "ly", "es", "s"}
	for _, suffix := range suffixes {
		if strings.HasSuffix(word, suffix) && len(word)-len(suffix) >= 3 {
			return word[:len(word)-len(suffix)]
		}
	}
	return word
}

// classifyKeywords maps keywords to IAB categories.
func classifyKeywords(keywords []string) []string {
	catScores := make(map[string]int)

	for _, kw := range keywords {
		lower := strings.ToLower(kw)
		for cat, words := range keywordMap {
			for _, w := range words {
				if strings.Contains(lower, w) {
					catScores[cat]++
				}
			}
		}
	}

	// Return categories with at least 1 match
	var result []string
	for cat, score := range catScores {
		if score > 0 {
			result = append(result, cat)
		}
	}
	return result
}

func defaultURLRules() []URLClassification {
	rules := []struct {
		pattern    string
		categories []string
	}{
		{`/sport`, []string{"IAB17"}},
		{`/football|/soccer|/premier.league`, []string{"IAB17", "IAB17-1"}},
		{`/tennis|/wimbledon`, []string{"IAB17", "IAB17-12"}},
		{`/finance|/money|/invest`, []string{"IAB13"}},
		{`/tech|/gadget|/software|/ai|/cloud`, []string{"IAB19"}},
		{`/auto|/car|/vehicle|/motor`, []string{"IAB2"}},
		{`/travel|/holiday|/flight|/hotel`, []string{"IAB20"}},
		{`/food|/recipe|/cooking|/restaurant`, []string{"IAB8"}},
		{`/health|/fitness|/wellness|/medical`, []string{"IAB7"}},
		{`/fashion|/style|/beauty|/clothing`, []string{"IAB18"}},
		{`/entertainment|/music|/movie|/celebrity`, []string{"IAB1"}},
		{`/news|/politics|/world|/breaking`, []string{"IAB12"}},
		{`/education|/learn|/university|/school`, []string{"IAB5"}},
		{`/property|/real.estate|/housing|/mortgage`, []string{"IAB10"}},
		{`/gaming|/game|/esport|/playstation|/xbox`, []string{"IAB9"}},
		{`/shopping|/deal|/sale|/discount|/coupon`, []string{"IAB22"}},
	}

	var result []URLClassification
	for _, r := range rules {
		result = append(result, URLClassification{
			Pattern:    regexp.MustCompile(r.pattern),
			Categories: r.categories,
		})
	}
	return result
}

var keywordMap = map[string][]string{
	"IAB17": {"sport", "football", "soccer", "cricket", "rugby", "tennis", "basketball", "match", "league", "championship"},
	"IAB13": {"finance", "invest", "stock", "market", "bank", "mortgage", "crypto", "bitcoin", "trading"},
	"IAB19": {"technology", "software", "gadget", "ai", "cloud", "startup", "programming", "developer", "silicon"},
	"IAB2":  {"car", "auto", "vehicle", "motor", "electric vehicle", "ev", "suv", "sedan"},
	"IAB20": {"travel", "holiday", "flight", "hotel", "vacation", "tourism", "destination"},
	"IAB12": {"news", "politics", "election", "government", "breaking", "world"},
	"IAB22": {"shopping", "deal", "sale", "discount", "price", "buy", "store"},
	"IAB9":  {"gaming", "game", "esport", "playstation", "xbox", "nintendo", "steam"},
}

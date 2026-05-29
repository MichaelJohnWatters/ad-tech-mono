package fraud

import (
	"testing"
	"time"
)

func TestRealTimeChecker_CleanRequest(t *testing.T) {
	checker := NewRealTimeChecker(DefaultConfig())
	result := checker.Check(Request{
		IP:        "192.168.1.1",
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/120.0",
		Referer:   "https://news.com/article",
		Timestamp: time.Now(),
	})

	if result.Blocked {
		t.Error("clean request should not be blocked")
	}
	if result.Score > 0.2 {
		t.Errorf("clean request score = %.2f, want < 0.2", result.Score)
	}
}

func TestRealTimeChecker_BotUA(t *testing.T) {
	checker := NewRealTimeChecker(DefaultConfig())
	result := checker.Check(Request{
		IP:        "192.168.1.1",
		UserAgent: "Googlebot/2.1 (+http://www.google.com/bot.html)",
		Timestamp: time.Now(),
	})

	if !result.Blocked {
		t.Error("bot should be blocked")
	}
	found := false
	for _, r := range result.Reasons {
		if r == "bot_user_agent" {
			found = true
		}
	}
	if !found {
		t.Error("expected bot_user_agent reason")
	}
}

func TestRealTimeChecker_BlockedIP(t *testing.T) {
	checker := NewRealTimeChecker(DefaultConfig())
	checker.BlockIP("10.0.0.99")

	result := checker.Check(Request{
		IP:        "10.0.0.99",
		UserAgent: "Mozilla/5.0",
		Timestamp: time.Now(),
	})

	if !result.Blocked {
		t.Error("blocked IP should be blocked")
	}

	checker.UnblockIP("10.0.0.99")
	result2 := checker.Check(Request{
		IP:        "10.0.0.99",
		UserAgent: "Mozilla/5.0",
		Referer:   "https://example.com",
		Timestamp: time.Now(),
	})
	if result2.Blocked {
		t.Error("unblocked IP should not be blocked")
	}
}

func TestRealTimeChecker_DataCenterIP(t *testing.T) {
	checker := NewRealTimeChecker(DefaultConfig())
	result := checker.Check(Request{
		IP:        "52.1.2.3", // AWS range
		UserAgent: "Mozilla/5.0",
		Referer:   "https://example.com",
		Timestamp: time.Now(),
	})

	found := false
	for _, r := range result.Reasons {
		if r == "data_center_ip" {
			found = true
		}
	}
	if !found {
		t.Error("expected data_center_ip reason for AWS IP")
	}
}

func TestRealTimeChecker_EmptyUA(t *testing.T) {
	checker := NewRealTimeChecker(DefaultConfig())
	result := checker.Check(Request{
		IP:        "192.168.1.1",
		UserAgent: "",
		Timestamp: time.Now(),
	})

	if result.Score < 0.3 {
		t.Errorf("empty UA score = %.2f, want >= 0.3", result.Score)
	}
}

func TestRealTimeChecker_RateLimit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxRequestsPerIPPerMinute = 3
	checker := NewRealTimeChecker(cfg)

	req := Request{IP: "10.0.0.1", UserAgent: "Mozilla/5.0", Referer: "https://x.com", Timestamp: time.Now()}

	// First 3 should pass
	for i := 0; i < 3; i++ {
		r := checker.Check(req)
		if r.Blocked {
			t.Errorf("request %d should not be blocked", i+1)
		}
	}

	// 4th should trigger rate limit
	r := checker.Check(req)
	found := false
	for _, reason := range r.Reasons {
		if reason == "rate_limited" {
			found = true
		}
	}
	if !found {
		t.Error("expected rate_limited after exceeding limit")
	}
}

func TestScorer(t *testing.T) {
	scorer := NewScorer(DefaultWeights())

	// Clean
	clean := scorer.Score(map[string]bool{})
	if clean.Category != "clean" {
		t.Errorf("clean category = %s, want clean", clean.Category)
	}

	// Bot + blocklist + rate limited + empty UA = SIVT
	sivt := scorer.Score(map[string]bool{"bot_ua": true, "ip_blocklist": true, "rate_limited": true, "empty_ua": true})
	if sivt.Category != "sivt" {
		t.Errorf("sivt category = %s, want sivt (score=%.2f)", sivt.Category, sivt.Score)
	}

	// Bot + blocklist only = GIVT (not enough signals for SIVT)
	givt := scorer.Score(map[string]bool{"bot_ua": true, "ip_blocklist": true})
	if givt.Category != "givt" {
		t.Errorf("givt category = %s, want givt (score=%.2f)", givt.Category, givt.Score)
	}
}

func TestParseAdsTxt(t *testing.T) {
	content := `# ads.txt for example.com
adtech.example, pub-123, DIRECT, abc123
google.com, pub-456, RESELLER
# comment line

invalid line
other-exchange.com, pub-789, DIRECT
`
	entries := ParseAdsTxt(content)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Domain != "adtech.example" {
		t.Errorf("domain = %s, want adtech.example", entries[0].Domain)
	}
	if entries[0].AccountID != "pub-123" {
		t.Errorf("account = %s, want pub-123", entries[0].AccountID)
	}
	if entries[0].Relationship != "DIRECT" {
		t.Errorf("relationship = %s, want DIRECT", entries[0].Relationship)
	}
	if entries[0].CertAuthority != "abc123" {
		t.Errorf("cert = %s, want abc123", entries[0].CertAuthority)
	}
}

func TestAdsTxtCache_IsAuthorised(t *testing.T) {
	cache := NewAdsTxtCache()

	entries := ParseAdsTxt("adtech.example, pub-123, DIRECT, abc123\ngoogle.com, pub-456, RESELLER")
	cache.Update("news.com", entries)

	// Authorised
	result := cache.IsAuthorised("news.com", "adtech.example", "pub-123")
	if !result.Authorised {
		t.Error("expected authorised")
	}
	if result.Relationship != "DIRECT" {
		t.Errorf("relationship = %s, want DIRECT", result.Relationship)
	}

	// Not listed
	result2 := cache.IsAuthorised("news.com", "adtech.example", "wrong-id")
	if result2.Authorised {
		t.Error("expected not authorised with wrong ID")
	}

	// No ads.txt
	result3 := cache.IsAuthorised("unknown.com", "adtech.example", "pub-123")
	if result3.Status != "no_ads_txt" {
		t.Errorf("status = %s, want no_ads_txt", result3.Status)
	}
}

func TestGenerateSellersJSON(t *testing.T) {
	sellers := GenerateSellersJSON([]SellerEntry{
		{SellerID: "pub-1", Name: "Acme Media", Domain: "acme.com", SellerType: "PUBLISHER"},
		{SellerID: "pub-2", Name: "News Corp", Domain: "news.com", SellerType: "PUBLISHER"},
	})

	if len(sellers.Sellers) != 2 {
		t.Errorf("sellers = %d, want 2", len(sellers.Sellers))
	}
}

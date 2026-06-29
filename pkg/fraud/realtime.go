// Package fraud provides real-time and batch fraud detection.
//
// Real-time checks run on every tracker request (< 1ms budget):
//   - Known bot user agents
//   - IP blocklist
//   - Rate limiting per IP
//   - Data center IP detection
//
// Batch scoring runs as a CronJob analysing patterns in analytics data.
//
// Usage:
//
//	checker := fraud.NewRealTimeChecker(fraud.DefaultConfig())
//	result := checker.Check(request)
//	if result.Blocked { /* reject */ }
package fraud

import (
	"net"
	"strings"
	"sync"
	"time"
)

// CheckResult is the output of a real-time fraud check.
type CheckResult struct {
	Blocked    bool
	Score      float64 // 0.0 (clean) to 1.0 (fraudulent)
	Reasons    []string
	CheckTime  time.Duration
}

// Request contains the signals for fraud evaluation.
type Request struct {
	IP        string
	UserAgent string
	TraceID   string
	Referer   string
	Timestamp time.Time
}

// Config holds fraud detection configuration.
type Config struct {
	// Rate limiting
	MaxRequestsPerIPPerMinute int
	// Score thresholds
	BlockThreshold float64 // score above this = blocked
	FlagThreshold  float64 // score above this = flagged for review
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		MaxRequestsPerIPPerMinute: 60,
		BlockThreshold:            0.7,
		FlagThreshold:             0.3,
	}
}

// BlocklistEntry is one DB-sourced fraud blocklist row (mirrors the
// fraud_blocklists table). Loaded into the tracker's warm cache and pushed
// into the checker via ReplaceBlocklists on every refresh.
type BlocklistEntry struct {
	Type  string // "ip" | "ua" | "domain" | "app_bundle"
	Value string
}

// RealTimeChecker performs fast fraud checks on incoming requests.
type RealTimeChecker struct {
	mu          sync.RWMutex
	config      Config
	ipBlocklist map[string]bool
	botPatterns []string
	dcRanges    []*net.IPNet // data center IP ranges
	rateCounts  map[string]*rateEntry

	// DB-sourced blocklists, replaced wholesale on each warm-cache refresh.
	// Kept separate from the hardcoded botPatterns and the manual
	// ipBlocklist (BlockIP) so a refresh never clobbers either.
	dbIPs        map[string]bool
	dbUAPatterns []string
}

type rateEntry struct {
	count    int
	windowStart time.Time
}

// NewRealTimeChecker creates a fraud checker with the given config.
func NewRealTimeChecker(cfg Config) *RealTimeChecker {
	return &RealTimeChecker{
		config:      cfg,
		ipBlocklist: make(map[string]bool),
		botPatterns: defaultBotPatterns(),
		dcRanges:    defaultDataCenterRanges(),
		rateCounts:  make(map[string]*rateEntry),
	}
}

// Check evaluates a request for fraud signals.
func (c *RealTimeChecker) Check(req Request) CheckResult {
	start := time.Now()
	var reasons []string
	score := 0.0

	// 1. IP blocklist
	if c.isBlockedIP(req.IP) {
		reasons = append(reasons, "ip_blocklisted")
		score += 0.9
	}

	// 2. Bot detection (user agent)
	if c.isBot(req.UserAgent) {
		reasons = append(reasons, "bot_user_agent")
		score += 0.8
	}

	// 3. Empty/missing user agent
	if req.UserAgent == "" {
		reasons = append(reasons, "empty_user_agent")
		score += 0.5
	}

	// 4. Data center IP
	if c.isDataCenterIP(req.IP) {
		reasons = append(reasons, "data_center_ip")
		score += 0.4
	}

	// 5. Rate limiting
	if c.isRateLimited(req.IP) {
		reasons = append(reasons, "rate_limited")
		score += 0.6
	}

	// 6. Missing referer (suspicious for impression pixels)
	if req.Referer == "" {
		reasons = append(reasons, "no_referer")
		score += 0.1
	}

	// Cap score at 1.0
	if score > 1.0 {
		score = 1.0
	}

	return CheckResult{
		Blocked:   score >= c.config.BlockThreshold,
		Score:     score,
		Reasons:   reasons,
		CheckTime: time.Since(start),
	}
}

// ReplaceBlocklists atomically swaps the DB-sourced IP set and UA patterns
// from a warm-cache snapshot. The hardcoded bot patterns, datacenter
// ranges, and any manually BlockIP'd addresses are unaffected — this only
// owns the rows that came from fraud_blocklists.
func (c *RealTimeChecker) ReplaceBlocklists(entries []BlocklistEntry) {
	ips := make(map[string]bool)
	var uas []string
	for _, e := range entries {
		switch e.Type {
		case "ip":
			ips[e.Value] = true
		case "ua":
			uas = append(uas, strings.ToLower(e.Value))
		}
	}
	c.mu.Lock()
	c.dbIPs = ips
	c.dbUAPatterns = uas
	c.mu.Unlock()
}

// BlockIP adds an IP to the blocklist.
func (c *RealTimeChecker) BlockIP(ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ipBlocklist[ip] = true
}

// UnblockIP removes an IP from the blocklist.
func (c *RealTimeChecker) UnblockIP(ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.ipBlocklist, ip)
}

// BlockedIPs returns the current blocklist.
func (c *RealTimeChecker) BlockedIPs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var ips []string
	for ip := range c.ipBlocklist {
		ips = append(ips, ip)
	}
	return ips
}

func (c *RealTimeChecker) isBlockedIP(ip string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ipBlocklist[ip] || c.dbIPs[ip]
}

func (c *RealTimeChecker) isBot(ua string) bool {
	lower := strings.ToLower(ua)
	// Hardcoded patterns are immutable after construction — lock-free read.
	for _, pattern := range c.botPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	// DB-sourced patterns are swapped under the lock by ReplaceBlocklists.
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, pattern := range c.dbUAPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func (c *RealTimeChecker) isDataCenterIP(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, cidr := range c.dcRanges {
		if cidr.Contains(parsed) {
			return true
		}
	}
	return false
}

func (c *RealTimeChecker) isRateLimited(ip string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	entry, ok := c.rateCounts[ip]
	if !ok || now.Sub(entry.windowStart) > time.Minute {
		c.rateCounts[ip] = &rateEntry{count: 1, windowStart: now}
		return false
	}
	entry.count++
	return entry.count > c.config.MaxRequestsPerIPPerMinute
}

func defaultBotPatterns() []string {
	return []string{
		"bot", "crawler", "spider", "scraper", "curl", "wget", "python-requests",
		"httpclient", "java/", "go-http-client", "headlesschrome", "phantomjs",
		"selenium", "puppeteer", "playwright", "scrapy", "nutch", "slurp",
		"bingbot", "googlebot", "yandexbot", "baiduspider", "duckduckbot",
	}
}

func defaultDataCenterRanges() []*net.IPNet {
	// Common cloud provider ranges (simplified)
	ranges := []string{
		"35.0.0.0/8",    // Google Cloud
		"34.0.0.0/8",    // Google Cloud
		"52.0.0.0/8",    // AWS
		"54.0.0.0/8",    // AWS
		"13.0.0.0/8",    // Azure
		"20.0.0.0/8",    // Azure
		"104.16.0.0/12", // Cloudflare
	}
	var nets []*net.IPNet
	for _, r := range ranges {
		_, cidr, err := net.ParseCIDR(r)
		if err == nil {
			nets = append(nets, cidr)
		}
	}
	return nets
}

package fraud

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AdsTxtRecord is a persisted ads.txt result for one publisher domain.
// Mirrors the ads_txt_cache table; loaded into the exchange's warm cache.
type AdsTxtRecord struct {
	Domain  string
	Entries []AdsTxtEntry
	Status  string // valid | missing | error
}

// FetchAdsTxt GETs https://{domain}/ads.txt and parses it. Status is
// "valid" (HTTP 200 + parsed), "missing" (404 — publisher has no ads.txt,
// which is a legitimate state, not an error), or "error" (network failure
// or unexpected status). The crawler (cmd/adstxt) upserts the result.
func FetchAdsTxt(ctx context.Context, client *http.Client, domain string) AdsTxtRecord {
	url := "https://" + domain + "/ads.txt"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return AdsTxtRecord{Domain: domain, Status: "error"}
	}
	resp, err := client.Do(req)
	if err != nil {
		return AdsTxtRecord{Domain: domain, Status: "error"}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return AdsTxtRecord{Domain: domain, Status: "missing"}
	case resp.StatusCode != http.StatusOK:
		return AdsTxtRecord{Domain: domain, Status: "error"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5 MiB cap per IAB guidance
	if err != nil {
		return AdsTxtRecord{Domain: domain, Status: "error"}
	}
	return AdsTxtRecord{Domain: domain, Entries: ParseAdsTxt(string(body)), Status: "valid"}
}

// AdsTxtEntry represents a single line in an ads.txt file.
type AdsTxtEntry struct {
	Domain        string // exchange domain
	AccountID     string // publisher's account ID on that exchange
	Relationship  string // DIRECT or RESELLER
	CertAuthority string // optional TAG-ID
}

// AdsTxtCache stores parsed ads.txt data per publisher domain.
type AdsTxtCache struct {
	mu      sync.RWMutex
	entries map[string][]AdsTxtEntry // domain -> entries
	fetched map[string]time.Time     // domain -> last fetched
}

// NewAdsTxtCache creates an empty ads.txt cache.
func NewAdsTxtCache() *AdsTxtCache {
	return &AdsTxtCache{
		entries: make(map[string][]AdsTxtEntry),
		fetched: make(map[string]time.Time),
	}
}

// ParseAdsTxt parses an ads.txt file content into entries.
func ParseAdsTxt(content string) []AdsTxtEntry {
	var entries []AdsTxtEntry
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}

		entry := AdsTxtEntry{
			Domain:       strings.TrimSpace(parts[0]),
			AccountID:    strings.TrimSpace(parts[1]),
			Relationship: strings.ToUpper(strings.TrimSpace(parts[2])),
		}
		if len(parts) >= 4 {
			entry.CertAuthority = strings.TrimSpace(parts[3])
		}
		entries = append(entries, entry)
	}
	return entries
}

// Update stores parsed ads.txt entries for a domain.
func (c *AdsTxtCache) Update(domain string, entries []AdsTxtEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[domain] = entries
	c.fetched[domain] = time.Now()
}

// IsAuthorised checks if our platform is listed as an authorised seller
// for the given publisher domain.
func (c *AdsTxtCache) IsAuthorised(publisherDomain, ourDomain, ourAccountID string) AdsTxtResult {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entries, ok := c.entries[publisherDomain]
	if !ok {
		return AdsTxtResult{Status: "no_ads_txt", Authorised: false}
	}

	for _, e := range entries {
		if strings.EqualFold(e.Domain, ourDomain) && e.AccountID == ourAccountID {
			return AdsTxtResult{
				Status:       "authorised",
				Authorised:   true,
				Relationship: e.Relationship,
			}
		}
	}

	return AdsTxtResult{Status: "not_listed", Authorised: false}
}

// Domains returns all cached domains.
func (c *AdsTxtCache) Domains() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var domains []string
	for d := range c.entries {
		domains = append(domains, d)
	}
	return domains
}

// AdsTxtResult is the outcome of an ads.txt authorisation check.
type AdsTxtResult struct {
	Status       string // authorised, not_listed, no_ads_txt
	Authorised   bool
	Relationship string // DIRECT, RESELLER
}

// SellerEntry represents a single seller in sellers.json.
type SellerEntry struct {
	SellerID      string `json:"seller_id"`
	Name          string `json:"name"`
	Domain        string `json:"domain"`
	SellerType    string `json:"seller_type"`    // PUBLISHER, INTERMEDIARY, BOTH
	IsPassthrough int    `json:"is_passthrough"` // 0 or 1
}

// SellersJSON represents the platform's sellers.json file.
type SellersJSON struct {
	ContactEmail string        `json:"contact_email"`
	ContactURL   string        `json:"contact_address"`
	Version      string        `json:"version"`
	Sellers      []SellerEntry `json:"sellers"`
}

// GenerateSellersJSON builds a sellers.json from registered publishers.
func GenerateSellersJSON(publishers []SellerEntry) SellersJSON {
	return SellersJSON{
		ContactEmail: "support@adtech.example",
		ContactURL:   "https://adtech.example/contact",
		Version:      fmt.Sprintf("1.0-%d", time.Now().Unix()),
		Sellers:      publishers,
	}
}

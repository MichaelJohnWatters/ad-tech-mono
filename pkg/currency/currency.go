// Package currency provides multi-currency conversion for the platform.
//
// Advertisers pay in USD, publishers earn in GBP, auctions clear in EUR.
// This package converts between any pair of supported currencies using
// daily exchange rates stored in Postgres and cached in L1.
//
// Every event record stores both original currency and USD equivalent
// so reporting can aggregate across currencies without re-converting.
//
// Usage:
//
//	converter := currency.NewConverter(rates)
//	gbp, err := converter.Convert(2.50, "USD", "GBP")
//	usdEquiv := converter.ToUSD(3.00, "GBP")
package currency

import (
	"fmt"
	"sync"
)

// Rate represents an exchange rate from base to target currency.
type Rate struct {
	Base   string  // ISO 4217 (e.g. "USD")
	Target string  // ISO 4217 (e.g. "GBP")
	Rate   float64 // 1 base = Rate target (e.g. 0.79)
}

// Converter handles currency conversion using cached exchange rates.
// Thread-safe: rates can be updated while conversions are happening.
type Converter struct {
	mu    sync.RWMutex
	rates map[string]float64 // "USD:GBP" -> 0.79
}

// NewConverter creates a Converter with the given rates.
func NewConverter(rates []Rate) *Converter {
	c := &Converter{
		rates: make(map[string]float64),
	}
	c.UpdateRates(rates)
	return c
}

// UpdateRates replaces all cached rates. Called when daily rates are refreshed
// or when NATS cache invalidation fires.
func (c *Converter) UpdateRates(rates []Rate) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.rates = make(map[string]float64, len(rates)*2)
	for _, r := range rates {
		key := r.Base + ":" + r.Target
		c.rates[key] = r.Rate
		// Also store the inverse
		inverseKey := r.Target + ":" + r.Base
		if r.Rate != 0 {
			c.rates[inverseKey] = 1.0 / r.Rate
		}
	}
	// Identity rates
	for _, r := range rates {
		c.rates[r.Base+":"+r.Base] = 1.0
		c.rates[r.Target+":"+r.Target] = 1.0
	}
}

// Convert converts an amount from one currency to another.
// Returns an error if the rate is not available.
func (c *Converter) Convert(amount float64, from, to string) (float64, error) {
	if from == to {
		return amount, nil
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	key := from + ":" + to
	rate, ok := c.rates[key]
	if !ok {
		// Try via USD as intermediary
		fromUSD, ok1 := c.rates[from+":USD"]
		usdTo, ok2 := c.rates["USD:"+to]
		if ok1 && ok2 {
			return amount * fromUSD * usdTo, nil
		}
		return 0, fmt.Errorf("no exchange rate for %s -> %s", from, to)
	}

	return amount * rate, nil
}

// ToUSD converts any amount to its USD equivalent.
// Used for normalised reporting across currencies.
func (c *Converter) ToUSD(amount float64, from string) (float64, error) {
	return c.Convert(amount, from, "USD")
}

// FromUSD converts a USD amount to the target currency.
func (c *Converter) FromUSD(amount float64, to string) (float64, error) {
	return c.Convert(amount, "USD", to)
}

// HasRate checks if a conversion rate exists for the given pair.
func (c *Converter) HasRate(from, to string) bool {
	if from == to {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := from + ":" + to
	if _, ok := c.rates[key]; ok {
		return true
	}
	// Check via USD intermediary
	_, ok1 := c.rates[from+":USD"]
	_, ok2 := c.rates["USD:"+to]
	return ok1 && ok2
}

// SupportedCurrencies returns all currencies that have at least one rate.
func (c *Converter) SupportedCurrencies() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	seen := make(map[string]bool)
	for key := range c.rates {
		for i := range key {
			if key[i] == ':' {
				seen[key[:i]] = true
				seen[key[i+1:]] = true
				break
			}
		}
	}

	result := make([]string, 0, len(seen))
	for curr := range seen {
		result = append(result, curr)
	}
	return result
}

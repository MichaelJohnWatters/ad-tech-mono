// Package billing handles spend calculation, revenue share, and the
// double-entry accounting ledger for the ad tech platform.
//
// The AuctionWinEvent is the single source of truth for cost.
// For CPM: bill immediately on win.
// For CPC/CPA/vCPM/CPCV: reserve on win, settle on click/conversion/view/complete.
//
// Usage:
//
//	engine := billing.NewEngine(ledger, contracts, clock, logger)
//	result, err := engine.ProcessImpression(ctx, event)
package billing

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

// BidModel is the billing model for a campaign.
type BidModel string

const (
	BidCPM  BidModel = "cpm"  // cost per mille - bill on impression
	BidCPC  BidModel = "cpc"  // cost per click - reserve on impression, settle on click
	BidCPA  BidModel = "cpa"  // cost per action - reserve on impression, settle on conversion
	BidVCPM BidModel = "vcpm" // viewable CPM - reserve on impression, settle on viewability
	BidCPCV BidModel = "cpcv" // cost per completed view - reserve on start, settle on complete
)

// SpendEvent is an incoming event that triggers billing.
type SpendEvent struct {
	TraceID        string
	CampaignID     string
	CreativeID     string
	PlacementID    string
	PublisherID    string
	AdvertiserID   string
	ClearingPrice  float64
	Currency       string
	BidModel       BidModel
	DealType       string // open, pmp, pg, preferred
	EventType      string // impression, click, conversion, viewable, complete
	Timestamp      time.Time
}

// SpendResult is the output of processing a spend event.
type SpendResult struct {
	TraceID           string
	AdvertiserSpend   float64
	PublisherRevenue  float64
	PlatformMargin    float64
	FeePercent        float64
	Subsidy           float64 // > 0 if guaranteed minimum applied
	Action            string  // "billed", "reserved", "settled", "released"
	ReservationID     string  // for reserve/settle models
}

// Engine processes billing events.
type Engine struct {
	ledger    *Ledger
	contracts *ContractStore
	clk       clock.Clock
	log       *slog.Logger
}

// NewEngine creates a billing engine.
func NewEngine(ledger *Ledger, contracts *ContractStore, clk clock.Clock, log *slog.Logger) *Engine {
	return &Engine{ledger: ledger, contracts: contracts, clk: clk, log: log}
}

// ProcessEvent handles any spend event according to its bid model.
func (e *Engine) ProcessEvent(ctx context.Context, event SpendEvent) (*SpendResult, error) {
	switch event.BidModel {
	case BidCPM:
		return e.billImmediate(ctx, event)
	case BidCPC:
		if event.EventType == "impression" {
			return e.reserve(ctx, event)
		} else if event.EventType == "click" {
			return e.settle(ctx, event)
		}
		return nil, nil // ignore other events for CPC
	case BidCPA:
		if event.EventType == "impression" {
			return e.reserve(ctx, event)
		} else if event.EventType == "conversion" {
			return e.settle(ctx, event)
		}
		return nil, nil
	case BidVCPM:
		if event.EventType == "impression" {
			return e.reserve(ctx, event)
		} else if event.EventType == "viewable" {
			return e.settle(ctx, event)
		}
		return nil, nil
	case BidCPCV:
		if event.EventType == "impression" {
			return e.reserve(ctx, event)
		} else if event.EventType == "complete" {
			return e.settle(ctx, event)
		}
		return nil, nil
	default:
		return e.billImmediate(ctx, event) // default to CPM
	}
}

func (e *Engine) billImmediate(ctx context.Context, event SpendEvent) (*SpendResult, error) {
	contract := e.contracts.Get(event.PublisherID)
	revenue := contract.CalculateRevenue(event.ClearingPrice, event.DealType)

	result := &SpendResult{
		TraceID:          event.TraceID,
		AdvertiserSpend:  event.ClearingPrice,
		PublisherRevenue: revenue.PublisherRevenue,
		PlatformMargin:   revenue.PlatformMargin,
		FeePercent:       revenue.FeePercent,
		Subsidy:          revenue.Subsidy,
		Action:           "billed",
	}

	// Write to ledger
	e.ledger.Record(LedgerEntry{
		Timestamp:        e.clk.Now(),
		TraceID:          event.TraceID,
		CampaignID:       event.CampaignID,
		PublisherID:      event.PublisherID,
		AdvertiserID:     event.AdvertiserID,
		Type:             EntrySpend,
		DebitAccount:     "advertiser:" + event.AdvertiserID,
		CreditAccount:    "publisher:" + event.PublisherID,
		Amount:           event.ClearingPrice,
		PublisherRevenue: revenue.PublisherRevenue,
		PlatformMargin:   revenue.PlatformMargin,
		Currency:         event.Currency,
	})

	e.log.Debug("billed",
		"trace_id", event.TraceID,
		"model", event.BidModel,
		"spend", event.ClearingPrice,
		"publisher_rev", revenue.PublisherRevenue,
		"margin", revenue.PlatformMargin,
	)

	return result, nil
}

func (e *Engine) reserve(ctx context.Context, event SpendEvent) (*SpendResult, error) {
	resID := fmt.Sprintf("res-%s", event.TraceID)

	e.ledger.Record(LedgerEntry{
		Timestamp:     e.clk.Now(),
		TraceID:       event.TraceID,
		CampaignID:    event.CampaignID,
		PublisherID:   event.PublisherID,
		AdvertiserID:  event.AdvertiserID,
		Type:          EntryReservation,
		DebitAccount:  "advertiser:" + event.AdvertiserID,
		CreditAccount: "escrow:" + resID,
		Amount:         event.ClearingPrice,
		Currency:       event.Currency,
		ReservationID:  resID,
	})

	e.log.Debug("reserved",
		"trace_id", event.TraceID,
		"model", event.BidModel,
		"amount", event.ClearingPrice,
		"reservation_id", resID,
	)

	return &SpendResult{
		TraceID:         event.TraceID,
		AdvertiserSpend: event.ClearingPrice,
		Action:          "reserved",
		ReservationID:   resID,
	}, nil
}

func (e *Engine) settle(ctx context.Context, event SpendEvent) (*SpendResult, error) {
	resID := fmt.Sprintf("res-%s", event.TraceID)
	contract := e.contracts.Get(event.PublisherID)
	revenue := contract.CalculateRevenue(event.ClearingPrice, event.DealType)

	// Settle: move from escrow to publisher
	e.ledger.Record(LedgerEntry{
		Timestamp:        e.clk.Now(),
		TraceID:          event.TraceID,
		CampaignID:       event.CampaignID,
		PublisherID:      event.PublisherID,
		AdvertiserID:     event.AdvertiserID,
		Type:             EntrySettlement,
		DebitAccount:     "escrow:" + resID,
		CreditAccount:    "publisher:" + event.PublisherID,
		Amount:           event.ClearingPrice,
		PublisherRevenue: revenue.PublisherRevenue,
		PlatformMargin:   revenue.PlatformMargin,
		Currency:         event.Currency,
		ReservationID:    resID,
	})

	e.log.Debug("settled",
		"trace_id", event.TraceID,
		"model", event.BidModel,
		"amount", event.ClearingPrice,
		"publisher_rev", revenue.PublisherRevenue,
	)

	return &SpendResult{
		TraceID:          event.TraceID,
		AdvertiserSpend:  event.ClearingPrice,
		PublisherRevenue: revenue.PublisherRevenue,
		PlatformMargin:   revenue.PlatformMargin,
		FeePercent:       revenue.FeePercent,
		Subsidy:          revenue.Subsidy,
		Action:           "settled",
		ReservationID:    resID,
	}, nil
}

// BalanceSummary returns spend/revenue totals for an account.
func (e *Engine) BalanceSummary(accountID string) BalanceSummary {
	return e.ledger.BalanceFor(accountID)
}

// BalanceSummary holds financial totals.
type BalanceSummary struct {
	AccountID    string
	TotalDebit   float64
	TotalCredit  float64
	Balance      float64
	Reservations float64
}

// ContractStore holds publisher revenue share contracts.
type ContractStore struct {
	mu        sync.RWMutex
	contracts map[string]*Contract
	fallback  *Contract
}

// NewContractStore creates a contract store with a default contract.
func NewContractStore() *ContractStore {
	return &ContractStore{
		contracts: make(map[string]*Contract),
		fallback: &Contract{
			Model:      ModelFixed,
			FeePct:     20,
			Currency:   "USD",
		},
	}
}

// Set stores a contract for a publisher.
func (s *ContractStore) Set(publisherID string, c *Contract) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contracts[publisherID] = c
}

// Get returns the contract for a publisher, or the default.
func (s *ContractStore) Get(publisherID string) *Contract {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.contracts[publisherID]; ok {
		return c
	}
	return s.fallback
}

// RevenueModel is the type of revenue share.
type RevenueModel string

const (
	ModelFixed      RevenueModel = "fixed"
	ModelTiered     RevenueModel = "tiered"
	ModelGuaranteed RevenueModel = "guaranteed"
	ModelDealType   RevenueModel = "deal_type"
	ModelHybrid     RevenueModel = "hybrid"
)

// Contract defines a publisher's revenue share terms.
type Contract struct {
	Model              RevenueModel
	FeePct             float64 // for fixed model
	Tiers              []Tier  // for tiered model
	GuaranteedMinCPM   float64 // for guaranteed model
	DealTypeModifiers  map[string]float64 // deal_type -> fee adjustment
	Currency           string
	MonthImpressions   int64 // current month's impression count (for tiered)
}

// Tier defines a volume-based fee tier.
type Tier struct {
	MinImpressions int64
	MaxImpressions int64 // 0 = unlimited
	FeePct         float64
}

// RevenueCalc holds the result of a revenue calculation.
type RevenueCalc struct {
	PublisherRevenue float64
	PlatformMargin   float64
	FeePercent       float64
	Subsidy          float64
}

// CalculateRevenue computes publisher revenue from clearing price.
func (c *Contract) CalculateRevenue(clearingPrice float64, dealType string) RevenueCalc {
	feePct := c.effectiveFee(dealType)
	pubRevenue := clearingPrice * (1 - feePct/100)
	margin := clearingPrice - pubRevenue
	subsidy := 0.0

	// Check guaranteed minimum
	if c.GuaranteedMinCPM > 0 && pubRevenue < c.GuaranteedMinCPM {
		subsidy = c.GuaranteedMinCPM - pubRevenue
		pubRevenue = c.GuaranteedMinCPM
		margin = clearingPrice - pubRevenue
	}

	return RevenueCalc{
		PublisherRevenue: pubRevenue,
		PlatformMargin:   margin,
		FeePercent:       feePct,
		Subsidy:          subsidy,
	}
}

func (c *Contract) effectiveFee(dealType string) float64 {
	baseFee := c.FeePct

	// For tiered model, look up tier
	if c.Model == ModelTiered || c.Model == ModelHybrid {
		for _, t := range c.Tiers {
			if c.MonthImpressions >= t.MinImpressions && (t.MaxImpressions == 0 || c.MonthImpressions < t.MaxImpressions) {
				baseFee = t.FeePct
				break
			}
		}
	}

	// Apply deal-type modifier
	if mod, ok := c.DealTypeModifiers[dealType]; ok {
		baseFee += mod
	}

	if baseFee < 0 {
		baseFee = 0
	}
	return baseFee
}

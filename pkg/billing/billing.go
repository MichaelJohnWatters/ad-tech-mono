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
	TraceID       string
	CampaignID    string
	CreativeID    string
	PlacementID   string
	PublisherID   string
	AdvertiserID  string
	ClearingPrice float64
	Currency      string
	BidModel      BidModel
	DealType      string // open, pmp, pg, preferred
	EventType     string // impression, click, conversion, viewable, complete
	Timestamp     time.Time
}

// SpendResult is the output of processing a spend event.
type SpendResult struct {
	TraceID          string
	AdvertiserSpend  float64
	PublisherRevenue float64
	PlatformMargin   float64
	FeePercent       float64
	Subsidy          float64 // > 0 if guaranteed minimum applied
	Action           string  // "billed", "reserved", "settled", "released"
	ReservationID    string  // for reserve/settle models
}

// BalanceSink applies realized advertiser spend to the prepay balance
// (advertiser_balances + spend ledger pair). Optional — nil means no
// drawdown (tests, deployments without prepay). Implementations must be
// idempotent on (traceID, eventType): NATS delivers at-least-once, and a
// replay must return applied=false rather than double-debit.
type BalanceSink interface {
	Debit(ctx context.Context, advertiserID string, amount float64, currency, traceID, eventType string) (newBalance float64, applied bool, err error)
}

// ReservationContext is the auction context of a reserve/settle reservation
// that a ledger backend may not retain. The MemoryLedger keeps it in-process;
// the TigerBeetle ledger stores only numeric IDs + amount + trace/bid_model,
// so the publisher/advertiser/campaign STRINGS are lost. Persisted on reserve
// and recovered on settle so settlement is backend-agnostic.
type ReservationContext struct {
	TraceID      string
	CampaignID   string
	CreativeID   string
	PlacementID  string
	PublisherID  string
	AdvertiserID string
	DealType     string
	Currency     string
	Amount       float64
	BidModel     string
}

// ReservationStore persists reservation context on reserve and recovers it
// on settle. Optional — nil means rely on the ledger alone (fine for the
// MemoryLedger, broken for TigerBeetle). Keyed by TraceID; Save must upsert
// (NATS redelivers reserves).
type ReservationStore interface {
	SaveReservation(ctx context.Context, rc ReservationContext) error
	GetReservation(ctx context.Context, traceID string) (ReservationContext, bool, error)
}

// Engine processes billing events.
type Engine struct {
	ledger       Ledger
	contracts    *ContractStore
	balances     BalanceSink      // optional; see SetBalanceSink
	reservations ReservationStore // optional; see SetReservationStore
	pacing       *pacingAccumulator
	clk          clock.Clock
	log          *slog.Logger
}

// SetBalanceSink connects the prepay drawdown: every realized spend
// (billImmediate + settle — the two places money becomes real) debits the
// advertiser's balance through the sink. Reserves do NOT touch the balance;
// they hold campaign budget, and settle is the realization point.
func (e *Engine) SetBalanceSink(s BalanceSink) { e.balances = s }

// SetReservationStore connects the settle-enrichment cache: reserve persists
// the auction context, settle recovers it when the ledger can't (TigerBeetle).
func (e *Engine) SetReservationStore(s ReservationStore) { e.reservations = s }

// drawdown pushes realized spend to the balance sink. Errors are logged at
// ERROR and never fail the billing event: the engine ledger row is already
// written (single source of truth), the sink is idempotent, and the next
// event or a reconciliation replay can recover the drawdown.
func (e *Engine) drawdown(ctx context.Context, event SpendEvent, action string) {
	if e.balances == nil {
		return
	}
	if event.AdvertiserID == "" || event.ClearingPrice <= 0 {
		return
	}
	if _, _, err := e.balances.Debit(ctx, event.AdvertiserID, event.ClearingPrice, event.Currency, event.TraceID, event.EventType); err != nil {
		e.log.Error("balance drawdown failed",
			"trace_id", event.TraceID, "advertiser_id", event.AdvertiserID,
			"amount", event.ClearingPrice, "action", action, "error", err)
	}
}

// NewEngine creates a billing engine. The ledger arg is an interface so
// callers can plug either MemoryLedger (dev/tests) or
// pkg/billing/tigerbeetle.Ledger (prod) without behaviour change.
func NewEngine(ledger Ledger, contracts *ContractStore, clk clock.Clock, log *slog.Logger) *Engine {
	return &Engine{ledger: ledger, contracts: contracts, pacing: newPacingAccumulator(clk), clk: clk, log: log}
}

// SnapshotCommitted returns today's committed spend (settled + open reserves),
// in cents, per campaign id. This is the authoritative pacing figure the
// reporting service publishes to DSPs so their budget gate reflects billed
// reality (phantom wins that never impressed are absent; reserves that never
// settle are swept) rather than the raw win prices the DSP counts locally.
// Campaign ids are the line-item UUIDs the DSP budget counter is keyed by.
func (e *Engine) SnapshotCommitted() map[string]int64 { return e.pacing.snapshot() }

// SweepExpiredHolds releases open reserves older than the pacing hold TTL
// (impressions whose billable settle never arrived) and returns the count
// released. Callers should invoke this on the same cadence as snapshotting.
func (e *Engine) SweepExpiredHolds() int { return e.pacing.sweepExpired() }

// SettledToday returns the durable settled-spend portion of committed (per
// campaign, in cents) plus the UTC day it belongs to. Reporting persists this
// each snapshot tick so a restart can HydrateSettled it back — without it, a
// restart resets committed to zero and reconciles DSP pacing counters down.
func (e *Engine) SettledToday() (string, map[string]int64) { return e.pacing.settledSnapshot() }

// HydrateSettled seeds the settled totals loaded from durable storage on boot.
// Only takes effect if day matches the current UTC day. Call before event
// consumption starts.
func (e *Engine) HydrateSettled(day string, cents map[string]int64) {
	e.pacing.hydrateSettled(day, cents)
}

// SetPacingHoldTTL overrides how long an open reserve counts toward committed
// spend before being swept. A non-positive duration is ignored (keeps the
// default). Live-tunable via the reporting service's config.
func (e *Engine) SetPacingHoldTTL(d time.Duration) {
	if d > 0 {
		e.pacing.holdTTL = d
	}
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

	e.drawdown(ctx, event, "billed")
	e.pacing.recordBilled(event.CampaignID, event.ClearingPrice)

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
		Amount:        event.ClearingPrice,
		Currency:      event.Currency,
		BidModel:      string(event.BidModel),
		DealType:      event.DealType,
		ReservationID: resID,
	})

	// Persist the auction context so settle can recover it even on a ledger
	// backend that doesn't retain strings (TigerBeetle). Best-effort — a
	// failure here means TB settles will fall back to the (empty) ledger
	// context, so log at ERROR but don't fail the reserve.
	if e.reservations != nil {
		if err := e.reservations.SaveReservation(ctx, ReservationContext{
			TraceID: event.TraceID, CampaignID: event.CampaignID, CreativeID: event.CreativeID,
			PlacementID: event.PlacementID, PublisherID: event.PublisherID,
			AdvertiserID: event.AdvertiserID, DealType: event.DealType,
			Currency: event.Currency, Amount: event.ClearingPrice, BidModel: string(event.BidModel),
		}); err != nil {
			e.log.Error("reservation context save failed",
				"trace_id", event.TraceID, "error", err)
		}
	}

	e.pacing.recordReserve(event.CampaignID, event.TraceID, event.ClearingPrice)

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

	e.drawdown(ctx, event, "settled")
	e.pacing.recordSettle(event.CampaignID, event.TraceID, event.ClearingPrice)

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

// SettleByTrace settles the open reservation for traceID using the given
// event type ("click" for CPC, "conversion" for CPA, "viewable" for vCPM,
// "complete" for CPCV). BidModel, ClearingPrice, PublisherID,
// AdvertiserID, CampaignID and DealType are recovered from the
// reservation row so callers (the reporting service's click/conversion
// consumers) don't have to thread the original auction context through
// the tracker URL params.
//
// Returns (nil, nil) when:
//   - no reservation exists for the trace (CPM impression, or impression
//     event hasn't been processed yet — NATS subjects deliver independently)
//   - the reservation has already been settled (defends against double-fire
//     when tracker dedup misses)
//   - the reservation's bid model doesn't match the event type (a click
//     on a CPA campaign shouldn't settle — only conversion does)
//
// All three cases are normal in steady state; the caller just acks and moves on.
func (e *Engine) SettleByTrace(ctx context.Context, traceID, eventType string) (*SpendResult, error) {
	res, ok := e.ledger.ReservationByTrace(traceID)
	if !ok {
		return nil, nil
	}
	if e.ledger.HasSettlement(traceID) {
		return nil, nil
	}
	if !settleEventMatches(BidModel(res.BidModel), eventType) {
		return nil, nil
	}
	settle := SpendEvent{
		TraceID:       traceID,
		CampaignID:    res.CampaignID,
		PublisherID:   res.PublisherID,
		AdvertiserID:  res.AdvertiserID,
		ClearingPrice: res.Amount,
		Currency:      res.Currency,
		BidModel:      BidModel(res.BidModel),
		DealType:      res.DealType,
		EventType:     eventType,
		Timestamp:     e.clk.Now(),
	}
	// The ledger keeps the money (amount/model) but a backend like TigerBeetle
	// can't retain the publisher/advertiser/campaign STRINGS. When they're
	// missing, recover them from the reservation store so both the ledger
	// settle record and the prepay drawdown have their account context.
	if e.reservations != nil && settle.AdvertiserID == "" {
		if rc, ok, err := e.reservations.GetReservation(ctx, traceID); err != nil {
			e.log.Error("reservation context lookup failed", "trace_id", traceID, "error", err)
		} else if ok {
			settle.CampaignID = rc.CampaignID
			settle.CreativeID = rc.CreativeID
			settle.PlacementID = rc.PlacementID
			settle.PublisherID = rc.PublisherID
			settle.AdvertiserID = rc.AdvertiserID
			settle.DealType = rc.DealType
			if settle.Currency == "" {
				settle.Currency = rc.Currency
			}
		}
	}
	return e.ProcessEvent(ctx, settle)
}

// settleEventMatches returns true if the event type would route through
// the settle branch of ProcessEvent for the given bid model. Keeps the
// mapping in one place so adding a new bid model means one new line.
func settleEventMatches(model BidModel, eventType string) bool {
	switch model {
	case BidCPC:
		return eventType == "click"
	case BidCPA:
		return eventType == "conversion"
	case BidVCPM:
		return eventType == "viewable"
	case BidCPCV:
		return eventType == "complete"
	}
	return false
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
			Model:    ModelFixed,
			FeePct:   20,
			Currency: "USD",
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
	Model             RevenueModel
	FeePct            float64            // for fixed model
	Tiers             []Tier             // for tiered model
	GuaranteedMinCPM  float64            // for guaranteed model
	DealTypeModifiers map[string]float64 // deal_type -> fee adjustment
	Currency          string
	MonthImpressions  int64 // current month's impression count (for tiered)
}

// Tier defines a volume-based fee tier. JSON tags match the revshare_config
// stored by the staff editor and decoded by the ContractLoader.
type Tier struct {
	MinImpressions int64   `json:"min_impressions"`
	MaxImpressions int64   `json:"max_impressions"` // 0 = unlimited
	FeePct         float64 `json:"fee_pct"`
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

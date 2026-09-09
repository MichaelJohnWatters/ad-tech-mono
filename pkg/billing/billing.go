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
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	TraceID      string
	CampaignID   string
	CreativeID   string
	PlacementID  string
	PublisherID  string
	AdvertiserID string
	// ClearingPrice is the realized per-impression cost in Currency (dollars),
	// NOT the auction CPM. Callers convert the CPM (bid.price = cost per 1000
	// impressions) to per-impression cost before ingestion — see
	// cmd/reporting normalizeImpressionCost — so a $5.00 CPM arrives as $0.005.
	// The ledger, pacing, and balance drawdown book this amount directly, and
	// reservations carry it through to settle unchanged (no re-conversion).
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
	// DebitBatch applies many realized spends in one round-trip (the scale
	// path). Same idempotency contract as Debit. Returns the post-decrement
	// balance for each advertiser that had NEW spend applied — replays that
	// inserted nothing are absent from the result.
	DebitBatch(ctx context.Context, debits []BatchDebit) ([]BatchDebitResult, error)
}

// BatchDebit is one realized spend for BalanceSink.DebitBatch.
type BatchDebit struct {
	AdvertiserID string
	Amount       float64
	Currency     string
	TraceID      string
	EventType    string
}

// BatchDebitResult is the post-decrement balance for an advertiser that had new
// spend applied in a DebitBatch call.
type BatchDebitResult struct {
	AdvertiserID string
	NewBalance   float64
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
	committed    CommittedCounter // optional; see SetCommittedCounter
	rates        RateSource       // optional; see SetRateSource
	pacing       *pacingAccumulator
	clk          clock.Clock
	log          *slog.Logger
}

// RateSource answers "how many units of `currency` is 1 USD worth on `on`?"
// (the exchange_rates table's convention: base USD, rate = target units per
// USD). ok=false means no rate is known for that currency/date.
type RateSource func(ctx context.Context, currency string, on time.Time) (rate float64, ok bool)

// SetRateSource wires multi-currency normalization: every SpendEvent whose
// Currency isn't USD is converted to USD before ANY money moves, so the
// ledger, balances, pacing and summaries stay single-currency (micro-DOLLAR
// truth). Without a source, non-USD events are refused rather than silently
// booked 1:1 — treating EUR as USD is money corruption.
func (e *Engine) SetRateSource(rs RateSource) { e.rates = rs }

// ErrNoExchangeRate marks a spend event refused because its currency can't
// be converted (no rate source wired, or no rate row for the currency/date).
var ErrNoExchangeRate = errors.New("no exchange rate for currency")

// normalizeCurrency converts event money to USD in place. USD/empty pass
// through untouched.
func (e *Engine) normalizeCurrency(ctx context.Context, event *SpendEvent) error {
	cur := strings.ToUpper(strings.TrimSpace(event.Currency))
	if cur == "" || cur == "USD" {
		event.Currency = "USD"
		return nil
	}
	if e.rates == nil {
		return fmt.Errorf("%w: %s (no rate source wired)", ErrNoExchangeRate, cur)
	}
	rate, ok := e.rates(ctx, cur, e.clk.Now())
	if !ok || rate <= 0 {
		return fmt.Errorf("%w: %s", ErrNoExchangeRate, cur)
	}
	e.log.Info("spend event currency normalized",
		"trace_id", event.TraceID, "from", cur, "amount", event.ClearingPrice, "rate", rate)
	event.ClearingPrice = event.ClearingPrice / rate
	event.Currency = "USD"
	return nil
}

// SetBalanceSink connects the prepay drawdown: every realized spend
// (billImmediate + settle — the two places money becomes real) debits the
// advertiser's balance through the sink. Reserves do NOT touch the balance;
// they hold campaign budget, and settle is the realization point.
func (e *Engine) SetBalanceSink(s BalanceSink) { e.balances = s }

// SetCommittedCounter connects the shared, cross-replica committed-spend store.
// When set, every committed change is mirrored to it (additive) and
// SnapshotCommitted reads from it instead of the local in-memory accumulator —
// so N reporting replicas each seeing part of the stream still produce the
// correct combined pacing snapshot. When nil (single replica / Redis down), the
// engine uses its in-memory accumulator exactly as before. See committed.go.
func (e *Engine) SetCommittedCounter(c CommittedCounter) { e.committed = c }

// emitCommitted mirrors a per-campaign committed delta to the shared counter.
// No-op when no counter is wired. Best-effort: a failed add is logged but never
// fails the billing event — the periodic store reconcile (Reconcile) resets the
// counter to authoritative truth, sweeping any dropped delta.
func (e *Engine) emitCommitted(ctx context.Context, deltas map[string]int64) {
	if e.committed == nil || len(deltas) == 0 {
		return
	}
	day := dayKey(e.clk.Now())
	if err := e.committed.AddDelta(ctx, day, deltas); err != nil {
		e.log.Error("committed counter add failed", "day", day, "campaigns", len(deltas), "error", err)
	}
}

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
// in micro-dollars (1 USD = 1e6 µ), per campaign id. This is the authoritative pacing figure the
// reporting service publishes to DSPs so their budget gate reflects billed
// reality (phantom wins that never impressed are absent; reserves that never
// settle are swept) rather than the raw win prices the DSP counts locally.
// Campaign ids are the line-item UUIDs the DSP budget counter is keyed by.
//
// When a shared CommittedCounter is wired (multi-replica reporting), the figure
// comes from it — the combined additive total across all pods — so no single
// pod's partial view leaks out. When nil, or if the counter read fails, it falls
// back to this pod's in-memory accumulator (single-replica behaviour, unchanged).
func (e *Engine) SnapshotCommitted() map[string]int64 {
	if e.committed != nil {
		day := dayKey(e.clk.Now())
		if snap, err := e.committed.Snapshot(context.Background(), day); err == nil {
			return snap
		} else {
			e.log.Error("committed counter snapshot failed; falling back to in-memory", "error", err)
		}
	}
	return e.pacing.snapshot()
}

// SweepExpiredHolds releases open reserves older than the pacing hold TTL
// (impressions whose billable settle never arrived) and returns the count
// released. Callers should invoke this on the same cadence as snapshotting. The
// freed budget is mirrored to the shared committed counter as a negative delta.
func (e *Engine) SweepExpiredHolds() int {
	released, deltas := e.pacing.sweepExpired()
	e.emitCommitted(context.Background(), deltas)
	return released
}

// SweepExpiredReservations releases open ledger reservations older than the
// pacing hold TTL — impressions whose billable settle event (click/conversion/
// view) never arrived. On the memory backend this reverses the escrow hold back
// to the advertiser (reserve never drew the prepay balance — only settle does —
// so it's a pure ledger reversal); the pacing hold that counts toward committed
// spend is freed separately by SweepExpiredHolds on the same TTL. On the
// TigerBeetle backend it's a no-op (TB auto-voids its pending transfers
// server-side). Runs on the same cadence as SweepExpiredHolds. Returns the
// count released.
func (e *Engine) SweepExpiredReservations() int {
	return e.ledger.ReleaseExpired(e.clk.Now(), e.pacing.holdTTL)
}

// PacingState returns the UTC day plus the persistable settled and open-reserved
// micro-dollars per campaign. Reporting persists this each snapshot tick so a restart
// can HydratePacing it back — without it, a restart resets committed to zero and
// reconciles DSP pacing counters down.
func (e *Engine) PacingState() (string, map[string]int64, map[string]int64) {
	return e.pacing.pacingState()
}

// HydratePacing seeds settled totals and restores open reserves loaded from
// durable storage on boot. Only takes effect if day matches the current UTC
// day. Call before event consumption starts.
func (e *Engine) HydratePacing(day string, settled, reserved map[string]int64) {
	e.pacing.hydrate(day, settled, reserved)
}

// ResetPacing clears the committed accumulator (settled + open reserves). For
// test isolation via the reporting debug billing-reset endpoint.
func (e *Engine) ResetPacing() { e.pacing.reset() }

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
	// Normalize to USD BEFORE any money moves — a refused conversion drops
	// the event loudly instead of booking a foreign amount as dollars.
	if err := e.normalizeCurrency(ctx, &event); err != nil {
		e.log.Error("spend event unbillable, dropped",
			"trace_id", event.TraceID, "currency", event.Currency, "error", err)
		return nil, err
	}
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

// ProcessBatch handles a batch of impression spend events in as few durable
// round-trips as possible — ONE ledger RecordBatch, ONE balance DebitBatch, and
// ONE pacing update — instead of N of each. It is the batched twin of
// ProcessEvent and produces the same per-event result and ledger entries; the
// per-entry idempotency keys (TB transfer IDs, the Postgres spend unique index)
// make a whole-batch replay safe. Settles (click/conversion/view) are NOT
// batched here: they arrive on separate subjects and go through SettleByTrace.
func (e *Engine) ProcessBatch(ctx context.Context, events []SpendEvent) ([]*SpendResult, error) {
	results := make([]*SpendResult, len(events))
	entries := make([]LedgerEntry, 0, len(events))
	var debits []BatchDebit
	pacingItems := make([]pacingItem, 0, len(events))
	now := e.clk.Now()

	for i := range events {
		event := events[i]
		// Only impressions are batched here. Anything else (shouldn't reach this
		// path from the impression consumer) falls back to the per-event path.
		if event.EventType != "" && event.EventType != "impression" {
			results[i], _ = e.ProcessEvent(ctx, event)
			continue
		}

		// Same USD normalization as ProcessEvent — one unbillable event is
		// dropped loudly without failing the rest of the batch.
		if err := e.normalizeCurrency(ctx, &event); err != nil {
			e.log.Error("spend event unbillable, dropped",
				"trace_id", event.TraceID, "currency", event.Currency, "error", err)
			continue
		}

		reserveModel := event.BidModel == BidCPC || event.BidModel == BidCPA ||
			event.BidModel == BidVCPM || event.BidModel == BidCPCV

		if reserveModel {
			// Reserve: hold budget in escrow, NO balance drawdown (billing.go
			// SetBalanceSink: reserves don't touch the balance; settle realizes).
			resID := fmt.Sprintf("res-%s", event.TraceID)
			entries = append(entries, LedgerEntry{
				Timestamp: now, TraceID: event.TraceID, CampaignID: event.CampaignID,
				PublisherID: event.PublisherID, AdvertiserID: event.AdvertiserID,
				Type: EntryReservation, DebitAccount: "advertiser:" + event.AdvertiserID,
				CreditAccount: "escrow:" + resID, Amount: event.ClearingPrice,
				Currency: event.Currency, BidModel: string(event.BidModel),
				DealType: event.DealType, ReservationID: resID,
			})
			if e.reservations != nil {
				if err := e.reservations.SaveReservation(ctx, ReservationContext{
					TraceID: event.TraceID, CampaignID: event.CampaignID, CreativeID: event.CreativeID,
					PlacementID: event.PlacementID, PublisherID: event.PublisherID,
					AdvertiserID: event.AdvertiserID, DealType: event.DealType,
					Currency: event.Currency, Amount: event.ClearingPrice, BidModel: string(event.BidModel),
				}); err != nil {
					e.log.Error("reservation context save failed", "trace_id", event.TraceID, "error", err)
				}
			}
			pacingItems = append(pacingItems, pacingItem{campaignID: event.CampaignID, traceID: event.TraceID, amount: event.ClearingPrice, kind: pacingReserve})
			results[i] = &SpendResult{TraceID: event.TraceID, AdvertiserSpend: event.ClearingPrice, Action: "reserved", ReservationID: resID}
			continue
		}

		// CPM (and default): bill immediately.
		contract := e.contracts.Get(event.PublisherID)
		revenue := contract.CalculateRevenue(event.ClearingPrice, event.DealType)
		entries = append(entries, LedgerEntry{
			Timestamp: now, TraceID: event.TraceID, CampaignID: event.CampaignID,
			PublisherID: event.PublisherID, AdvertiserID: event.AdvertiserID,
			Type: EntrySpend, DebitAccount: "advertiser:" + event.AdvertiserID,
			CreditAccount: "publisher:" + event.PublisherID, Amount: event.ClearingPrice,
			PublisherRevenue: revenue.PublisherRevenue, PlatformMargin: revenue.PlatformMargin,
			Currency: event.Currency,
		})
		if event.AdvertiserID != "" && event.ClearingPrice > 0 {
			debits = append(debits, BatchDebit{
				AdvertiserID: event.AdvertiserID, Amount: event.ClearingPrice,
				Currency: event.Currency, TraceID: event.TraceID, EventType: event.EventType,
			})
		}
		pacingItems = append(pacingItems, pacingItem{campaignID: event.CampaignID, amount: event.ClearingPrice, kind: pacingBilled})
		results[i] = &SpendResult{
			TraceID: event.TraceID, AdvertiserSpend: event.ClearingPrice,
			PublisherRevenue: revenue.PublisherRevenue, PlatformMargin: revenue.PlatformMargin,
			FeePercent: revenue.FeePercent, Subsidy: revenue.Subsidy, Action: "billed",
		}
	}

	if len(entries) > 0 {
		e.ledger.RecordBatch(entries)
	}
	if len(debits) > 0 && e.balances != nil {
		if _, err := e.balances.DebitBatch(ctx, debits); err != nil {
			// Best-effort, exactly like drawdown: the ledger is the source of
			// truth and the sink is idempotent, so a reconciliation replay
			// recovers a failed batch. Never fails the events.
			e.log.Error("balance batch drawdown failed", "debits", len(debits), "error", err)
		}
	}
	e.emitCommitted(ctx, e.pacing.recordBatch(pacingItems))

	return results, nil
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
	e.emitCommitted(ctx, map[string]int64{event.CampaignID: e.pacing.recordBilled(event.CampaignID, event.ClearingPrice)})

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

	e.emitCommitted(ctx, map[string]int64{event.CampaignID: e.pacing.recordReserve(event.CampaignID, event.TraceID, event.ClearingPrice)})

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
	e.emitCommitted(ctx, map[string]int64{event.CampaignID: e.pacing.recordSettle(event.CampaignID, event.TraceID, event.ClearingPrice)})

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

// SettleRequest is one settle to attempt in a batch: the trace whose open
// reservation should settle, and the event type that triggers it
// (click/conversion/viewable/complete).
type SettleRequest struct {
	TraceID   string
	EventType string
}

// ProcessSettleBatch settles many reservations in as few durable round-trips as
// possible — ONE ledger RecordBatch + ONE balance DebitBatch + ONE pacing update
// — the settle-path twin of ProcessBatch. It is what keeps the CPC/CPA/vCPM/CPCV
// trigger events (clicks/conversions/views, high volume in a mixed-model stream)
// from bottlenecking ingestion on per-event TigerBeetle settle chains.
//
// The per-trace decisions are IDENTICAL to SettleByTrace — reservation lookup,
// already-settled dedup, bid-model/event matching, and context recovery from the
// reservation store — so batching changes only how the WRITES are issued, not
// what gets settled. Returns one result per request, nil where the settle was a
// no-op (no reservation yet, already settled, model mismatch, or a duplicate
// trace within this same batch).
func (e *Engine) ProcessSettleBatch(ctx context.Context, reqs []SettleRequest) ([]*SpendResult, error) {
	results := make([]*SpendResult, len(reqs))
	entries := make([]LedgerEntry, 0, len(reqs))
	var debits []BatchDebit
	pacingItems := make([]pacingItem, 0, len(reqs))
	now := e.clk.Now()
	seen := make(map[string]bool, len(reqs)) // collapse duplicate triggers within the batch

	for i := range reqs {
		traceID, eventType := reqs[i].TraceID, reqs[i].EventType
		if traceID == "" || seen[traceID] {
			continue
		}
		res, ok := e.ledger.ReservationByTrace(traceID)
		if !ok {
			continue
		}
		if e.ledger.HasSettlement(traceID) {
			continue
		}
		if !settleEventMatches(BidModel(res.BidModel), eventType) {
			continue
		}

		settle := SpendEvent{
			TraceID: traceID, CampaignID: res.CampaignID, PublisherID: res.PublisherID,
			AdvertiserID: res.AdvertiserID, ClearingPrice: res.Amount, Currency: res.Currency,
			BidModel: BidModel(res.BidModel), DealType: res.DealType, EventType: eventType, Timestamp: now,
		}
		// Recover the account STRINGS the ledger can't retain (TigerBeetle), same
		// as SettleByTrace, so the settle record + drawdown have their context.
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
		seen[traceID] = true

		resID := fmt.Sprintf("res-%s", traceID)
		contract := e.contracts.Get(settle.PublisherID)
		revenue := contract.CalculateRevenue(settle.ClearingPrice, settle.DealType)
		entries = append(entries, LedgerEntry{
			Timestamp: now, TraceID: traceID, CampaignID: settle.CampaignID,
			PublisherID: settle.PublisherID, AdvertiserID: settle.AdvertiserID,
			Type: EntrySettlement, DebitAccount: "escrow:" + resID,
			CreditAccount: "publisher:" + settle.PublisherID, Amount: settle.ClearingPrice,
			PublisherRevenue: revenue.PublisherRevenue, PlatformMargin: revenue.PlatformMargin,
			Currency: settle.Currency, ReservationID: resID,
		})
		if settle.AdvertiserID != "" && settle.ClearingPrice > 0 {
			debits = append(debits, BatchDebit{
				AdvertiserID: settle.AdvertiserID, Amount: settle.ClearingPrice,
				Currency: settle.Currency, TraceID: traceID, EventType: eventType,
			})
		}
		pacingItems = append(pacingItems, pacingItem{campaignID: settle.CampaignID, traceID: traceID, amount: settle.ClearingPrice, kind: pacingSettle})
		results[i] = &SpendResult{
			TraceID: traceID, AdvertiserSpend: settle.ClearingPrice,
			PublisherRevenue: revenue.PublisherRevenue, PlatformMargin: revenue.PlatformMargin,
			FeePercent: revenue.FeePercent, Subsidy: revenue.Subsidy,
			Action: "settled", ReservationID: resID,
		}
	}

	if len(entries) > 0 {
		e.ledger.RecordBatch(entries)
	}
	if len(debits) > 0 && e.balances != nil {
		if _, err := e.balances.DebitBatch(ctx, debits); err != nil {
			// Best-effort, like ProcessBatch: the ledger is the source of truth and
			// the sink is idempotent, so a reconciliation replay recovers a failed
			// batch. Never fails the events.
			e.log.Error("settle batch drawdown failed", "debits", len(debits), "error", err)
		}
	}
	e.emitCommitted(ctx, e.pacing.recordBatch(pacingItems))

	return results, nil
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
	// monthImps is the publisher's month-to-date impression count, kept SEPARATE
	// from the parsed contract (fee/tiers): the contract config comes from the
	// warm cache (which re-parses periodically, resetting struct fields), while
	// the count comes from a distinct analytics refresh. Get merges it in so a
	// tiered fee is chosen off it, without the two refreshers clobbering each
	// other.
	monthImps map[string]int64
	fallback  *Contract
}

// NewContractStore creates a contract store with a default contract.
func NewContractStore() *ContractStore {
	return &ContractStore{
		contracts: make(map[string]*Contract),
		monthImps: make(map[string]int64),
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

// SetMonthImpressions records the publisher's running month-to-date impression
// count, used to pick a tiered revenue-share tier at settle time. Sourced from
// the analytics store (cluster-global), refreshed independently of the contract
// config. See Get.
func (s *ContractStore) SetMonthImpressions(publisherID string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.monthImps[publisherID] = n
}

// Get returns the contract for a publisher, or the default — as a COPY carrying
// the current month's impression count, so the tiered fee lookup sees it. A
// copy (not the shared pointer) keeps the count merge race-free against a
// concurrent warm-cache Set and lets callers read it without mutating the
// stored contract.
func (s *ContractStore) Get(publisherID string) *Contract {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.fallback
	if got, ok := s.contracts[publisherID]; ok {
		c = got
	}
	cp := *c
	cp.MonthImpressions = s.monthImps[publisherID]
	return &cp
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

// CalculateRevenue computes publisher revenue from a realized amount in
// per-impression dollars (post money-precision: the tracker books CPM/1000, so
// every ledger amount reaching this is per-impression, not a CPM).
func (c *Contract) CalculateRevenue(clearingPrice float64, dealType string) RevenueCalc {
	feePct := c.effectiveFee(dealType)
	pubRevenue := clearingPrice * (1 - feePct/100)
	margin := clearingPrice - pubRevenue
	subsidy := 0.0

	// Guaranteed minimum. The contract rate is CPM-denominated (per 1000
	// impressions — that's how the staff editor and revshare_config express
	// it), but the amount being split here is per-impression dollars, so the
	// floor must be applied at per-impression scale. Comparing against the
	// raw CPM figure paid publishers the full per-mille floor on EVERY
	// impression (a 1000× overpay) once amounts became per-impression.
	if minPerImp := c.GuaranteedMinCPM / 1000; minPerImp > 0 && pubRevenue < minPerImp {
		subsidy = minPerImp - pubRevenue
		pubRevenue = minPerImp
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

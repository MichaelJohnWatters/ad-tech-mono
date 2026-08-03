// Package auction provides the unified auction engine with pluggable strategies.
//
// The Exchange selects a strategy based on the impression type, runs the common
// filter pipeline, then delegates to the strategy for winner selection.
//
// Strategies (all implemented):
//   - SingleWinner: display, native, video single, rewarded, interstitial
//   - Pod: video/audio ad breaks with multiple winners
//   - RelevanceWeighted: retail sponsored products, relevance * bid
//   - Batch: in-game intrinsic scene surfaces, competitive separation
//   - TimeSlot: DOOH screen rotation slots
package auction

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

var (
	// ErrNotImplemented is returned by stubbed strategies.
	ErrNotImplemented = errors.New("auction strategy not yet implemented")

	// ErrNoBids is returned when no eligible bids remain after filtering.
	ErrNoBids = errors.New("no eligible bids")

	// ErrBelowFloor is returned when all bids are below the floor price.
	ErrBelowFloor = errors.New("all bids below floor price")
)

// Strategy is the interface all auction strategies implement.
type Strategy interface {
	// Select winners from filtered, eligible bids.
	Select(ctx context.Context, bids []Bid, request AuctionRequest) (Result, error)

	// Type returns the strategy type for logging/metrics.
	Type() string
}

// Bid represents a single bid from a DSP.
type Bid struct {
	DSPID        string
	CampaignID   string
	CreativeID   string
	Price        float64
	Currency     string
	BidModel     string // cpm, cpc, cpa, vcpm, cpcv, cpi
	Duration     int    // seconds (for video/audio)
	Width        int    // creative pixel width
	Height       int    // creative pixel height
	MediaURL     string // video / audio media file URL — passed through to the winning OpenRTB BidObj.MediaURL
	AdM          string // ad markup — native response JSON (OpenRTB Native), passed through to the winning BidObj.AdM
	AdvertiserID string
	// SettlementSeat is the TRUSTED billable seat for data-fee attribution, set
	// by the exchange from WHICH configured endpoint returned this bid (never the
	// self-declared response seat). "" for internal demand we own. Not part of the
	// auction math — carried through so the winner's response can expose it.
	SettlementSeat string
	AdomainHost    string // first entry of OpenRTB BidObj.ADomain — preserved here so the exchange can carry the
	                   // advertiser landing domain into the winner response without re-querying the DSP
	Category     string // IAB category for competitive separation
	// Relevance is an optional 0..1 quality/relevance score for retail media
	// (sponsored-product ranking). When >0 the RelevanceWeighted strategy ranks
	// by Relevance×Price using this value directly; when 0 it derives relevance
	// from this bid's Category against the request's RetailCategories. Ignored by
	// every other strategy.
	Relevance    float64
	DealID       string // if bidding on a specific deal
	ResponseTime time.Duration
}

// AuctionRequest contains the context for an auction.
type AuctionRequest struct {
	RequestID     string
	PlacementID   string
	PublisherID   string
	PageRequestID string  // groups slots on same page (competitive separation)
	Channel       string  // display, video, audio, dooh, retail, ingame
	Format        string  // banner, native, video, audio
	FloorPrice    float64
	FloorCurrency string
	PriceMode     string // first_price, second_price
	DealIDs       []string
	TraceID       string
	// Retail media (relevance_weighted) knobs. SlotCount is the number of
	// sponsored-product slots on the results/browse page (1..N; 0 → 1).
	// RetailCategories are the shopper's browsed/searched IAB categories — the
	// relevance signal a sponsored product's Bid.Category is scored against.
	// Both zero/empty for every other channel.
	SlotCount        int
	RetailCategories []string
	// Pod carries pod-specific constraints when this is an ad-break
	// auction (video/audio Format == "pod"). nil for single-winner
	// auctions. Populated by the exchange when it sees Imp.Video.PodID
	// or Imp.Audio.PodID on the incoming bid request.
	Pod *PodRequest
}

// PodRequest is the pod-specific bit of an AuctionRequest. Mirrors the
// pod fields from OpenRTB 2.6 (Imp.Video / Imp.Audio: PodID, MaxSeq,
// RqdDurs, MinCPMPerSec) plus competitive-separation knobs.
//
// Two pod shapes are supported:
//
//   - Variable-duration ("free-form" pod): MaxDuration is set, RqdDurs
//     is empty. The auction picks any combination of bids whose total
//     duration fits within MaxDuration. Real-world example: a 60s
//     mid-roll break that can hold 2×30s, 4×15s, 1×30s+2×15s, etc.
//
//   - Fixed-slot pod: RqdDurs is non-empty. Each slot has a required
//     duration; the auction picks one bid per slot whose Duration
//     matches. Real-world example: SSAI pods on CTV where the SSAI
//     server has pre-allocated stitched slots of specific durations.
type PodRequest struct {
	PodID         string  // pod identifier (shared across all slots in the pod)
	MaxDuration   int     // seconds; total pod duration cap (variable-duration pods)
	RqdDurs       []int   // seconds per slot (fixed-slot pods); zero entries match any duration
	MaxAds        int     // 0 = no limit; otherwise refuse to fill more than N slots
	MinCPMPerSec  float64 // per-second floor across the pod
	NoSameAdvertiserAdjacent bool // when true, two adjacent slots cannot share AdvertiserID (industry default for instream pods)
	UniqueAdvertiser         bool // when true, advertiser can appear at most once anywhere in the pod
	UniqueCategory           bool // when true, IAB category can appear at most once anywhere in the pod
}

// Result contains the auction outcome.
type Result struct {
	Winners       []Winner
	LossBids      []LossBid
	ShortFill     int    // unfilled duration (pods) or slots (batch/timeslot)
	IsMultiWinner bool   // true for pod, batch, timeslot, retail
	IsRotation    bool   // true for DOOH timeslot
	StrategyType  string // for logging and metrics
}

// Winner represents an auction winner.
type Winner struct {
	Bid           Bid
	ClearingPrice float64
	Position      int // for retail (1,2,3) or pod/timeslot slot index
	TraceID       string
}

// LossBid records why a bid lost.
type LossBid struct {
	Bid        Bid
	Reason     int    // OpenRTB loss reason code
	ReasonText string // human-readable reason
}

// Loss reason codes (OpenRTB standard).
const (
	LossInternal       = 1
	LossTimeout        = 2
	LossInvalidBid     = 3
	LossBelowFloor     = 100
	LossBelowDealFloor = 101
	LossOutbid         = 102
	LossBlocked        = 103
	LossCreativeReject = 104
)

// Engine is the main auction engine that selects and runs strategies.
type Engine struct {
	strategies map[string]Strategy
	clk        clock.Clock
}

// NewEngine creates an auction engine with registered strategies.
func NewEngine(clk clock.Clock) *Engine {
	e := &Engine{
		strategies: make(map[string]Strategy),
		clk:        clk,
	}
	// Register all strategies
	e.strategies["single_winner"] = &SingleWinnerStrategy{}
	e.strategies["pod"] = &PodStrategy{}
	e.strategies["relevance_weighted"] = &RelevanceWeightedStrategy{}
	e.strategies["batch"] = &BatchStrategy{}
	e.strategies["timeslot"] = &TimeSlotStrategy{}
	return e
}

// SelectStrategy picks the appropriate strategy for the given request.
func (e *Engine) SelectStrategy(request AuctionRequest) (Strategy, error) {
	switch request.Channel {
	case "display", "native":
		return e.strategies["single_winner"], nil
	case "video", "audio":
		// Single ad = single_winner, ad break = pod
		if request.Format == "pod" {
			return e.strategies["pod"], nil
		}
		return e.strategies["single_winner"], nil
	case "dooh":
		return e.strategies["timeslot"], nil
	case "retail":
		return e.strategies["relevance_weighted"], nil
	case "ingame":
		if request.Format == "intrinsic" {
			return e.strategies["batch"], nil
		}
		return e.strategies["single_winner"], nil
	default:
		return e.strategies["single_winner"], nil
	}
}

// RunAuction executes the full auction pipeline: filter -> strategy -> result.
func (e *Engine) RunAuction(ctx context.Context, bids []Bid, request AuctionRequest) (Result, error) {
	start := e.clk.Now()

	strategy, err := e.SelectStrategy(request)
	if err != nil {
		return Result{}, fmt.Errorf("select strategy: %w", err)
	}

	// Common filter pipeline (all strategies)
	eligible := filterBids(bids, request)
	if len(eligible) == 0 {
		return Result{
			StrategyType: strategy.Type(),
			LossBids:     allLossBids(bids, request),
		}, ErrNoBids
	}

	// Delegate to strategy
	result, err := strategy.Select(ctx, eligible, request)
	if err != nil {
		return result, err
	}

	result.StrategyType = strategy.Type()

	// Add loss bids for filtered-out bids
	result.LossBids = append(result.LossBids, filterLossBids(bids, eligible, request)...)

	_ = e.clk.Since(start) // auction duration for metrics

	return result, nil
}

// filterBids removes bids that don't meet basic eligibility.
func filterBids(bids []Bid, request AuctionRequest) []Bid {
	var eligible []Bid
	for _, bid := range bids {
		// Below floor price
		if bid.Price < request.FloorPrice {
			continue
		}
		eligible = append(eligible, bid)
	}
	return eligible
}

// allLossBids creates loss bids for all bids when no one is eligible.
func allLossBids(bids []Bid, request AuctionRequest) []LossBid {
	var losses []LossBid
	for _, bid := range bids {
		reason := LossOutbid
		reasonText := "no eligible bids"
		if bid.Price < request.FloorPrice {
			reason = LossBelowFloor
			reasonText = "bid below floor price"
		}
		losses = append(losses, LossBid{Bid: bid, Reason: reason, ReasonText: reasonText})
	}
	return losses
}

// filterLossBids creates loss bids for bids that were filtered out.
func filterLossBids(all, eligible []Bid, request AuctionRequest) []LossBid {
	eligibleSet := make(map[string]bool, len(eligible))
	for _, b := range eligible {
		eligibleSet[b.DSPID+":"+b.CampaignID] = true
	}

	var losses []LossBid
	for _, bid := range all {
		key := bid.DSPID + ":" + bid.CampaignID
		if !eligibleSet[key] {
			reason := LossBelowFloor
			reasonText := "bid below floor price"
			if bid.Price >= request.FloorPrice {
				reason = LossBlocked
				reasonText = "blocked by quality controls"
			}
			losses = append(losses, LossBid{Bid: bid, Reason: reason, ReasonText: reasonText})
		}
	}
	return losses
}

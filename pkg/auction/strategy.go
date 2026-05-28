// Package auction provides the unified auction engine with pluggable strategies.
//
// The Exchange selects a strategy based on the impression type, runs the common
// filter pipeline, then delegates to the strategy for winner selection.
//
// Strategies:
//   - SingleWinner: display, native, video single, rewarded, interstitial (implemented)
//   - Pod: video/audio ad breaks with multiple winners (stubbed, Phase 9)
//   - RelevanceWeighted: retail sponsored products, relevance * bid (stubbed, Phase 9)
//   - Batch: in-game intrinsic billboards, multiple placements (stubbed, Phase 9)
//   - TimeSlot: DOOH screen rotation slots (stubbed, Phase 9)
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
	AdvertiserID string
	Category     string // IAB category for competitive separation
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

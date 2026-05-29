package events

import "time"

// AuctionWinEvent is published by the Exchange after an auction completes.
// Single source of truth for cost. Consumed by DSP (budget) and Reporting (billing).
type AuctionWinEvent struct {
	TraceID       string  `json:"trace_id"`
	AuctionID     string  `json:"auction_id"`
	WinnerDSP     string  `json:"winner_dsp"`
	CampaignID    string  `json:"campaign_id"`
	CreativeID    string  `json:"creative_id"`
	PlacementID   string  `json:"placement_id"`
	PublisherID   string  `json:"publisher_id"`
	AdvertiserID  string  `json:"advertiser_id"`
	ClearingPrice float64 `json:"clearing_price"`
	Currency      string  `json:"currency"`
	BidModel      string  `json:"bid_model"`
	DealID        string  `json:"deal_id,omitempty"`
	Channel       string  `json:"channel"`
	Timestamp     time.Time `json:"timestamp"`
}

// AuctionCompleteEvent includes all bids and timing (for analytics).
type AuctionCompleteEvent struct {
	TraceID       string         `json:"trace_id"`
	PlacementID   string         `json:"placement_id"`
	PublisherID   string         `json:"publisher_id"`
	Channel       string         `json:"channel"`
	NumBids       int            `json:"num_bids"`
	WinnerDSP     string         `json:"winner_dsp,omitempty"`
	ClearingPrice float64        `json:"clearing_price,omitempty"`
	FloorPrice    float64        `json:"floor_price"`
	DurationMs    int64          `json:"duration_ms"`
	Bids          []BidSummary   `json:"bids,omitempty"`
	Timestamp     time.Time      `json:"timestamp"`
}

// BidSummary is a single bid in the auction complete event.
type BidSummary struct {
	DSPID      string  `json:"dsp_id"`
	CampaignID string  `json:"campaign_id"`
	Price      float64 `json:"price"`
	Won        bool    `json:"won"`
}

// BudgetDepletedEvent is published by the DSP when a campaign runs out of budget.
type BudgetDepletedEvent struct {
	CampaignID string    `json:"campaign_id"`
	AccountID  string    `json:"account_id"`
	Budget     float64   `json:"budget"`
	Spent      float64   `json:"spent"`
	Timestamp  time.Time `json:"timestamp"`
}

// CampaignStateEvent is published when a campaign changes state.
type CampaignStateEvent struct {
	CampaignID string    `json:"campaign_id"`
	AccountID  string    `json:"account_id"`
	OldState   string    `json:"old_state"`
	NewState   string    `json:"new_state"`
	Reason     string    `json:"reason,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

// OptOutEvent is published when a user opts out.
type OptOutEvent struct {
	UserID    string    `json:"user_id"`
	Level     int       `json:"level"` // 1, 2, or 3
	Source    string    `json:"source"`
	Timestamp time.Time `json:"timestamp"`
}

// CacheInvalidateEvent tells services to clear their L1 cache for a resource.
type CacheInvalidateEvent struct {
	ResourceType string `json:"resource_type"` // campaign, placement, creative, dsp-endpoint
	ResourceID   string `json:"resource_id"`
	Action       string `json:"action"` // update, delete
}

package analytics

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryStore is an in-memory analytics store for testing.
// Not suitable for production - no persistence, no SQL.
type MemoryStore struct {
	mu          sync.RWMutex
	impressions []ImpressionEvent
	clicks      []ClickEvent
	conversions []ConversionEvent
	views       []ViewEvent
	auctions    []AuctionEvent
	auctionWins []AuctionWinEvent
	// Operational signal counters — not bid events, but observable
	// state transitions the analytics layer surfaces to dashboards.
	budgetDepletions     []BudgetDepletion
	serveNoFills         []ServeNoFill
	mediaEvents          []MediaEvent
	campaignStateChanges []CampaignStateChange
	trackerRejections    []TrackerRejection
	renderFailures       []RenderFailure
	freqCapBlocks        []FreqCapBlock
}

// FreqCapBlock records a serve suppression — adserver's
// (user, campaign) freq-cap counter was saturated, the impression
// pixel never fired. Lets ops alert on suppression-rate change and
// gives advertisers visibility into over-cap volume.
type FreqCapBlock struct {
	TraceID     string
	UserID      string
	CampaignID  string
	PlacementID string
	PublisherID string
	Timestamp   time.Time
}

// RenderFailure records an ad server fallback — the requested creative
// couldn't be resolved (unknown_creative) or rendered (render_error),
// so the server fell back to placeholder HTML. The impression still
// fires, so without this record the failure is invisible to billing
// and to advertiser dashboards.
type RenderFailure struct {
	TraceID     string
	CampaignID  string
	CreativeID  string
	PlacementID string
	PublisherID string
	Reason      string // unknown_creative / render_error / asset_missing
	Detail      string
	Timestamp   time.Time
}

// TrackerRejection records a pixel dropped at the gate (invalid HMAC,
// fraud block, dedup hit). Produced by reporting's
// adtech.tracker.rejected consumer; used by ops fraud-volume alerts
// and advertiser "we blocked X% of fraudulent traffic" reporting.
type TrackerRejection struct {
	TraceID   string
	EventType string // impression / click / conversion / view
	Reason    string // invalid_signature / fraud / dedup
	Detail    string
	Timestamp time.Time
}

// CampaignStateChange records a campaign transitioning between
// live/paused/archived/ended. Produced by reporting's
// adtech.campaign.state_changed consumer; used by ops dashboards to
// show pause/resume timelines + by e2e tests to assert the event
// actually propagated.
type CampaignStateChange struct {
	CampaignID string
	AccountID  string
	OldState   string
	NewState   string
	Reason     string
	Timestamp  time.Time
}

// MediaEvent records a video or audio engagement ping (VAST/DAAST event).
// One bucket for both formats with `Channel` distinguishing them — the
// shape is identical and analytics queries are typically grouped by
// (channel, event_type).
type MediaEvent struct {
	TraceID    string
	Channel    string // "video" or "audio"
	EventType  string
	PositionMs int64
	Timestamp  time.Time
}

// BudgetDepletion records the moment a DSP detected a campaign had
// exhausted its daily budget. One record per (campaign, pod lifetime)
// produced by the DSP's bidHandler.
type BudgetDepletion struct {
	CampaignID string
	AccountID  string
	Budget     float64
	Spent      float64
	Timestamp  time.Time
}

// ServeNoFill records a publisher-adserver request that fell through
// every demand source (direct miss + programmatic nobid + house miss).
// Counterpart to AuctionWinEvent — required for fill-rate computation.
type ServeNoFill struct {
	TraceID     string
	PublisherID string
	PlacementID string
	Reason      string // "no-direct-and-no-programmatic-and-no-house" etc.
	Timestamp   time.Time
}

// NewMemory creates an in-memory analytics store.
func NewMemory() *MemoryStore {
	return &MemoryStore{}
}

func (s *MemoryStore) InsertImpression(_ context.Context, e *ImpressionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.impressions = append(s.impressions, *e)
	return nil
}

func (s *MemoryStore) InsertClick(_ context.Context, e *ClickEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clicks = append(s.clicks, *e)
	return nil
}

func (s *MemoryStore) InsertConversion(_ context.Context, e *ConversionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conversions = append(s.conversions, *e)
	return nil
}

func (s *MemoryStore) InsertView(_ context.Context, e *ViewEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.views = append(s.views, *e)
	return nil
}

func (s *MemoryStore) InsertAuction(_ context.Context, e *AuctionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auctions = append(s.auctions, *e)
	return nil
}

func (s *MemoryStore) InsertAuctionWin(_ context.Context, e *AuctionWinEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auctionWins = append(s.auctionWins, *e)
	return nil
}

// AuctionWinCount returns how many auction-win records have been recorded
// for the given trace_id. Used by tests to verify dedup / once-only delivery.
func (s *MemoryStore) AuctionWinCount(traceID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, w := range s.auctionWins {
		if w.TraceID == traceID {
			n++
		}
	}
	return n
}

// InsertBudgetDepletion appends a depletion record. Called by reporting's
// adtech.budget.depleted consumer.
func (s *MemoryStore) InsertBudgetDepletion(d BudgetDepletion) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.budgetDepletions = append(s.budgetDepletions, d)
}

// InsertFreqCapBlock appends a suppression record. Called by
// reporting's adtech.adserver.freq_cap_blocked consumer.
func (s *MemoryStore) InsertFreqCapBlock(b FreqCapBlock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.freqCapBlocks = append(s.freqCapBlocks, b)
}

// FreqCapBlocksByCampaign returns the recorded suppression records
// for a campaign. Used by ops dashboards and e2e tests.
func (s *MemoryStore) FreqCapBlocksByCampaign(campaignID string) []FreqCapBlock {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []FreqCapBlock
	for _, b := range s.freqCapBlocks {
		if b.CampaignID == campaignID {
			out = append(out, b)
		}
	}
	return out
}

// InsertRenderFailure appends an ad-server render-failure record.
// Called by reporting's adtech.adserver.render_failed consumer.
func (s *MemoryStore) InsertRenderFailure(r RenderFailure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renderFailures = append(s.renderFailures, r)
}

// RenderFailuresByCreative returns the recorded failures for a
// creative_id. Used by ops dashboards to surface "creative X broke
// 47 times in the last hour" + by e2e tests.
func (s *MemoryStore) RenderFailuresByCreative(creativeID string) []RenderFailure {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []RenderFailure
	for _, r := range s.renderFailures {
		if r.CreativeID == creativeID {
			out = append(out, r)
		}
	}
	return out
}

// InsertTrackerRejection appends a rejection record. Called by
// reporting's adtech.tracker.rejected consumer.
func (s *MemoryStore) InsertTrackerRejection(r TrackerRejection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trackerRejections = append(s.trackerRejections, r)
}

// TrackerRejectionsByTrace returns the rejection records for a
// trace_id, optionally filtered by reason (empty reason = all). Used
// by e2e tests to verify a specific rejection class propagated and by
// ops to drill into a suspicious trace.
func (s *MemoryStore) TrackerRejectionsByTrace(traceID, reason string) []TrackerRejection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []TrackerRejection
	for _, r := range s.trackerRejections {
		if r.TraceID != traceID {
			continue
		}
		if reason != "" && r.Reason != reason {
			continue
		}
		out = append(out, r)
	}
	return out
}

// TrackerRejectionsByReason counts the total rejections recorded for
// a given reason ("invalid_signature" / "fraud" / "dedup"). Used by
// the ops fraud-volume dashboard.
func (s *MemoryStore) TrackerRejectionsByReason(reason string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, r := range s.trackerRejections {
		if r.Reason == reason {
			n++
		}
	}
	return n
}

// InsertCampaignStateChange appends a state-transition record.
// Called by reporting's adtech.campaign.state_changed consumer.
func (s *MemoryStore) InsertCampaignStateChange(c CampaignStateChange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.campaignStateChanges = append(s.campaignStateChanges, c)
}

// CampaignStateChangesByCampaign returns the recorded transitions for a
// given campaign_id, in event-arrival order. Used by e2e tests to assert
// that a pause/resume actually propagated through the bus.
func (s *MemoryStore) CampaignStateChangesByCampaign(campaignID string) []CampaignStateChange {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []CampaignStateChange
	for _, c := range s.campaignStateChanges {
		if c.CampaignID == campaignID {
			out = append(out, c)
		}
	}
	return out
}

// BudgetDepletionsByCampaign returns how many depletion records have been
// recorded for a given campaign_id. Used by e2e tests + ops dashboards
// to verify the event flow.
func (s *MemoryStore) BudgetDepletionsByCampaign(campaignID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, d := range s.budgetDepletions {
		if d.CampaignID == campaignID {
			n++
		}
	}
	return n
}

// InsertServeNoFill appends a no-fill record. Called by reporting's
// adtech.serve.nofill consumer. Required for fill-rate computation —
// AuctionWin records the numerator, ServeNoFill the denominator's miss.
func (s *MemoryStore) InsertServeNoFill(n ServeNoFill) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serveNoFills = append(s.serveNoFills, n)
}

// ServeNoFillsByTrace returns how many no-fill records exist for the
// trace. Used by e2e tests; ops queries would aggregate by publisher
// or placement instead.
func (s *MemoryStore) ServeNoFillsByTrace(traceID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, x := range s.serveNoFills {
		if x.TraceID == traceID {
			n++
		}
	}
	return n
}

// InsertMediaEvent implements the Store interface for video / audio
// engagement records (called by reporting's adtech.events.video and
// adtech.events.audio consumers). Context is accepted for interface
// symmetry; MemoryStore ignores it because the append is always
// synchronous and never blocks.
func (s *MemoryStore) InsertMediaEvent(_ context.Context, e *MediaEvent) error {
	if e == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mediaEvents = append(s.mediaEvents, *e)
	return nil
}

// MediaEventsByTrace counts media events for a trace, optionally filtered
// by channel ("video" / "audio") and event_type. Empty filters match all.
func (s *MemoryStore) MediaEventsByTrace(traceID, channel, eventType string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, e := range s.mediaEvents {
		if e.TraceID != traceID {
			continue
		}
		if channel != "" && e.Channel != channel {
			continue
		}
		if eventType != "" && e.EventType != eventType {
			continue
		}
		n++
	}
	return n
}

// AuctionWinByBidModel filters AuctionWins for a given trace_id by their
// BidModel value. Used by e2e tests to distinguish records produced by
// the standard programmatic path (bid_model="cpm"/"cpc"/...) from the
// direct-sold (bid_model="direct:sponsorship" etc.) and outbound-Prebid
// (bid_model="prebid_outbound") paths, all of which share the same
// underlying analytics table.
func (s *MemoryStore) AuctionWinByBidModel(traceID, bidModel string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, w := range s.auctionWins {
		if w.TraceID == traceID && w.BidModel == bidModel {
			n++
		}
	}
	return n
}

func (s *MemoryStore) InsertBatch(ctx context.Context, events []Event) error {
	for _, e := range events {
		var err error
		switch e.Type {
		case EventImpression:
			err = s.InsertImpression(ctx, e.Impression)
		case EventClick:
			err = s.InsertClick(ctx, e.Click)
		case EventConversion:
			err = s.InsertConversion(ctx, e.Conversion)
		case EventView:
			err = s.InsertView(ctx, e.View)
		case EventAuction:
			err = s.InsertAuction(ctx, e.Auction)
		default:
			return fmt.Errorf("unknown event type: %s", e.Type)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *MemoryStore) Query(_ context.Context, params QueryParams) (*QueryResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	switch params.Table {
	case "impressions":
		return s.queryImpressions(params)
	case "clicks":
		return s.queryClicks(params)
	case "conversions":
		return s.queryConversions(params)
	case "views":
		return s.queryViews(params)
	case "auctions":
		return s.queryAuctions(params)
	default:
		return nil, fmt.Errorf("unknown table: %s", params.Table)
	}
}

func (s *MemoryStore) Close() error { return nil }

// Counts returns event counts for assertions in tests.
func (s *MemoryStore) Counts() (impressions, clicks, conversions, auctions int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.impressions), len(s.clicks), len(s.conversions), len(s.auctions)
}

// Views returns all stored view events for test assertions.
func (s *MemoryStore) Views() []ViewEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ViewEvent, len(s.views))
	copy(out, s.views)
	return out
}

// Impressions returns all stored impressions for test assertions.
func (s *MemoryStore) Impressions() []ImpressionEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ImpressionEvent, len(s.impressions))
	copy(out, s.impressions)
	return out
}

func (s *MemoryStore) queryImpressions(params QueryParams) (*QueryResult, error) {
	// Filter by time range and filters
	var filtered []ImpressionEvent
	for _, imp := range s.impressions {
		if !params.TimeFrom.IsZero() && imp.Timestamp.Before(params.TimeFrom) {
			continue
		}
		if !params.TimeTo.IsZero() && imp.Timestamp.After(params.TimeTo) {
			continue
		}
		if !matchFilters(params.Filters, map[string]string{
			"trace_id":     imp.TraceID,
			"campaign_id":  imp.CampaignID,
			"creative_id":  imp.CreativeID,
			"placement_id": imp.PlacementID,
			"publisher_id": imp.PublisherID,
			"account_id":   imp.AccountID,
			"geo":          imp.Geo,
			"device":       imp.Device,
			"channel":      imp.Channel,
		}) {
			continue
		}
		filtered = append(filtered, imp)
	}

	// If no dimensions, aggregate across all filtered rows
	if len(params.Dimensions) == 0 {
		return s.aggregateImpressions(filtered, params.Metrics, nil)
	}

	// Group by dimensions
	groups := map[string][]ImpressionEvent{}
	for _, imp := range filtered {
		key := dimensionKey(params.Dimensions, map[string]string{
			"trace_id":     imp.TraceID,
			"campaign_id":  imp.CampaignID,
			"creative_id":  imp.CreativeID,
			"placement_id": imp.PlacementID,
			"publisher_id": imp.PublisherID,
			"account_id":   imp.AccountID,
			"geo":          imp.Geo,
			"device":       imp.Device,
			"channel":      imp.Channel,
			"day":          imp.Timestamp.Format("2006-01-02"),
			"hour":         imp.Timestamp.Format("2006-01-02T15"),
		})
		groups[key] = append(groups[key], imp)
	}

	return s.aggregateImpressionGroups(groups, params)
}

func (s *MemoryStore) aggregateImpressions(imps []ImpressionEvent, metrics []string, dimValues []string) (*QueryResult, error) {
	cols := make([]string, 0, len(dimValues)+len(metrics))
	row := make([]interface{}, 0, len(dimValues)+len(metrics))

	for _, v := range dimValues {
		row = append(row, v)
	}

	for _, m := range metrics {
		switch m {
		case "count":
			cols = append(cols, "count")
			row = append(row, int64(len(imps)))
		case "sum_cost":
			cols = append(cols, "sum_cost")
			var sum float64
			for _, imp := range imps {
				sum += imp.ClearingPriceUSD
			}
			row = append(row, sum)
		default:
			return nil, fmt.Errorf("unknown metric: %s", m)
		}
	}

	return &QueryResult{Columns: cols, Rows: [][]interface{}{row}}, nil
}

func (s *MemoryStore) aggregateImpressionGroups(groups map[string][]ImpressionEvent, params QueryParams) (*QueryResult, error) {
	var columns []string
	columns = append(columns, params.Dimensions...)
	columns = append(columns, params.Metrics...)

	var rows [][]interface{}
	for key, imps := range groups {
		dimValues := strings.Split(key, "|")
		row := make([]interface{}, 0, len(dimValues)+len(params.Metrics))
		for _, v := range dimValues {
			row = append(row, v)
		}
		for _, m := range params.Metrics {
			switch m {
			case "count":
				row = append(row, int64(len(imps)))
			case "sum_cost":
				var sum float64
				for _, imp := range imps {
					sum += imp.ClearingPriceUSD
				}
				row = append(row, sum)
			}
		}
		rows = append(rows, row)
	}

	// Sort for deterministic test output
	sort.Slice(rows, func(i, j int) bool {
		return fmt.Sprint(rows[i][0]) < fmt.Sprint(rows[j][0])
	})

	if params.Limit > 0 && len(rows) > params.Limit {
		rows = rows[:params.Limit]
	}

	return &QueryResult{Columns: columns, Rows: rows}, nil
}

func (s *MemoryStore) queryClicks(params QueryParams) (*QueryResult, error) {
	var count int64
	for _, c := range s.clicks {
		if !params.TimeFrom.IsZero() && c.Timestamp.Before(params.TimeFrom) {
			continue
		}
		if !params.TimeTo.IsZero() && c.Timestamp.After(params.TimeTo) {
			continue
		}
		if !matchFilters(params.Filters, map[string]string{
			"trace_id":     c.TraceID,
			"campaign_id":  c.CampaignID,
			"placement_id": c.PlacementID,
			"account_id":   c.AccountID,
		}) {
			continue
		}
		count++
	}
	return &QueryResult{
		Columns: []string{"count"},
		Rows:    [][]interface{}{{count}},
	}, nil
}

func (s *MemoryStore) queryConversions(params QueryParams) (*QueryResult, error) {
	var count int64
	var totalRevenue float64
	for _, c := range s.conversions {
		if !params.TimeFrom.IsZero() && c.Timestamp.Before(params.TimeFrom) {
			continue
		}
		if !params.TimeTo.IsZero() && c.Timestamp.After(params.TimeTo) {
			continue
		}
		if !matchFilters(params.Filters, map[string]string{
			"trace_id":    c.TraceID,
			"campaign_id": c.CampaignID,
			"account_id":  c.AccountID,
		}) {
			continue
		}
		count++
		totalRevenue += c.RevenueUSD
	}
	return &QueryResult{
		Columns: []string{"count", "sum_revenue"},
		Rows:    [][]interface{}{{count, totalRevenue}},
	}, nil
}

func (s *MemoryStore) queryViews(params QueryParams) (*QueryResult, error) {
	var count, viewableCount int64
	var totalDur int64
	for _, v := range s.views {
		if !params.TimeFrom.IsZero() && v.Timestamp.Before(params.TimeFrom) {
			continue
		}
		if !params.TimeTo.IsZero() && v.Timestamp.After(params.TimeTo) {
			continue
		}
		if !matchFilters(params.Filters, map[string]string{
			"trace_id":     v.TraceID,
			"campaign_id":  v.CampaignID,
			"creative_id":  v.CreativeID,
			"placement_id": v.PlacementID,
			"publisher_id": v.PublisherID,
			"account_id":   v.AccountID,
		}) {
			continue
		}
		count++
		totalDur += v.DurationMs
		if v.IABViewable {
			viewableCount++
		}
	}
	var avgDur float64
	var viewableRate float64
	if count > 0 {
		avgDur = float64(totalDur) / float64(count)
		viewableRate = float64(viewableCount) / float64(count)
	}
	return &QueryResult{
		Columns: []string{"count", "viewable_count", "viewable_rate", "avg_duration_ms"},
		Rows:    [][]interface{}{{count, viewableCount, viewableRate, avgDur}},
	}, nil
}

func (s *MemoryStore) queryAuctions(params QueryParams) (*QueryResult, error) {
	var count int64
	var totalDuration int64
	for _, a := range s.auctions {
		if !params.TimeFrom.IsZero() && a.Timestamp.Before(params.TimeFrom) {
			continue
		}
		if !params.TimeTo.IsZero() && a.Timestamp.After(params.TimeTo) {
			continue
		}
		if !matchFilters(params.Filters, map[string]string{
			"trace_id":     a.TraceID,
			"placement_id": a.PlacementID,
			"publisher_id": a.PublisherID,
			"channel":      a.Channel,
		}) {
			continue
		}
		count++
		totalDuration += a.DurationMs
	}
	var avgDuration float64
	if count > 0 {
		avgDuration = float64(totalDuration) / float64(count)
	}
	return &QueryResult{
		Columns: []string{"count", "avg_duration_ms"},
		Rows:    [][]interface{}{{count, avgDuration}},
	}, nil
}

func matchFilters(filters map[string]string, fields map[string]string) bool {
	for k, v := range filters {
		if fv, ok := fields[k]; ok && fv != v {
			return false
		}
	}
	return true
}

func dimensionKey(dims []string, fields map[string]string) string {
	parts := make([]string, len(dims))
	for i, d := range dims {
		parts[i] = fields[d]
	}
	return strings.Join(parts, "|")
}

// timeToDay and timeToHour for dimension grouping
func timeToDay(t time.Time) string  { return t.Format("2006-01-02") }
func timeToHour(t time.Time) string { return t.Format("2006-01-02T15") }

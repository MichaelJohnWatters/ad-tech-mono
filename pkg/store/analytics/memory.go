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
	rollups              []RollupRow
	dspCalls             []DSPCallEvent
	// Profile-store tables (ADR 0006 phase 1) — CI/test parity for the two
	// new BatchInserter methods (append-only, same as the event slices).
	behaviourSignals       []BehaviourSignalRow
	profileSignals         []ProfileSignalRow
	attributionTouchpoints []AttributionTouchpointRow
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

// InsertRollups persists aggregated rollup rows. Implements RollupWriter.
// Idempotent by (config, level, window_from): re-running a rollup for the
// same window REPLACES the prior rows for that window rather than
// duplicating them — so a re-run (retry, overlapping schedule) doesn't
// double-count. Each rollup.runOne batch is a single (config, level,
// window), so this replaces exactly that window's rows.
func (s *MemoryStore) InsertRollups(_ context.Context, rows []RollupRow) error {
	if len(rows) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	type winKey struct {
		config, level string
		from          int64
	}
	replacing := make(map[winKey]bool, len(rows))
	for _, r := range rows {
		replacing[winKey{r.Config, r.Level, r.WindowFrom.UnixNano()}] = true
	}
	kept := make([]RollupRow, 0, len(s.rollups))
	for _, existing := range s.rollups {
		if !replacing[winKey{existing.Config, existing.Level, existing.WindowFrom.UnixNano()}] {
			kept = append(kept, existing)
		}
	}
	s.rollups = append(kept, rows...)
	return nil
}

// QueryRollups returns stored rollup rows for (config, level) whose window
// overlaps [from, to] (RollupReader). Zero from/to means unbounded on that
// side. Returns copies so callers can't mutate the store's slice.
func (s *MemoryStore) QueryRollups(_ context.Context, config, level string, from, to time.Time) ([]RollupRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []RollupRow
	for _, r := range s.rollups {
		if r.Config != config || r.Level != level {
			continue
		}
		if !to.IsZero() && !r.WindowFrom.Before(to) {
			continue // window starts at/after the range end
		}
		if !from.IsZero() && !r.WindowTo.After(from) {
			continue // window ends at/before the range start
		}
		out = append(out, r)
	}
	return out, nil
}

// RollupCount returns how many rollup rows have been stored for a given
// (config, level). Used by tests/ops to verify the rollup engine wrote.
func (s *MemoryStore) RollupCount(config string, level string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, r := range s.rollups {
		if r.Config == config && r.Level == level {
			n++
		}
	}
	return n
}

// Rollups returns a copy of all stored rollup rows for test assertions.
func (s *MemoryStore) Rollups() []RollupRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]RollupRow, len(s.rollups))
	copy(out, s.rollups)
	return out
}

// --- BatchInserter parity (loops single-row inserts) ---

func (s *MemoryStore) InsertImpressions(ctx context.Context, es []*ImpressionEvent) error {
	for _, e := range es {
		if err := s.InsertImpression(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (s *MemoryStore) InsertClicks(ctx context.Context, es []*ClickEvent) error {
	for _, e := range es {
		if err := s.InsertClick(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (s *MemoryStore) InsertConversions(ctx context.Context, es []*ConversionEvent) error {
	for _, e := range es {
		if err := s.InsertConversion(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (s *MemoryStore) InsertViews(ctx context.Context, es []*ViewEvent) error {
	for _, e := range es {
		if err := s.InsertView(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (s *MemoryStore) InsertAuctions(ctx context.Context, es []*AuctionEvent) error {
	for _, e := range es {
		if err := s.InsertAuction(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (s *MemoryStore) InsertAuctionWins(ctx context.Context, es []*AuctionWinEvent) error {
	for _, e := range es {
		if err := s.InsertAuctionWin(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (s *MemoryStore) InsertMediaEvents(ctx context.Context, es []*MediaEvent) error {
	for _, e := range es {
		if err := s.InsertMediaEvent(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (s *MemoryStore) InsertDSPCalls(_ context.Context, es []*DSPCallEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range es {
		s.dspCalls = append(s.dspCalls, *e)
	}
	return nil
}

// InsertBehaviourSignals appends consent-gated behavioural rows (ADR 0006 phase
// 1) — memory-backend parity for the ClickHouse bulk insert.
func (s *MemoryStore) InsertBehaviourSignals(_ context.Context, es []*BehaviourSignalRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range es {
		if e == nil {
			continue
		}
		s.behaviourSignals = append(s.behaviourSignals, *e)
	}
	return nil
}

// ViewableImpressionsForUsers is the memory-backend view-through lookback (see
// the ClickHouse impl): behaviour_signals impressions for the user set + account
// (optionally one campaign) since `since`, with viewability recovered by
// correlating the views slice on trace_id. Most-recent first.
func (s *MemoryStore) ViewableImpressionsForUsers(_ context.Context, userIDs []string, accountID, campaignID string, since time.Time, requireViewable bool) ([]ViewableImpression, error) {
	if len(userIDs) == 0 || accountID == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	want := make(map[string]bool, len(userIDs))
	for _, u := range userIDs {
		want[u] = true
	}
	viewable := make(map[string]bool)
	for i := range s.views {
		if s.views[i].IABViewable {
			viewable[s.views[i].TraceID] = true
		}
	}
	var out []ViewableImpression
	for i := range s.behaviourSignals {
		b := &s.behaviourSignals[i]
		// Match on user id OR household id (the resolved set can hold both).
		if b.Kind != "impression" || b.AccountID != accountID || !(want[b.UserID] || want[b.HouseholdID]) {
			continue
		}
		if b.ObservedAt.Before(since) {
			continue
		}
		if campaignID != "" && b.CampaignID != campaignID {
			continue
		}
		vw := viewable[b.TraceID]
		if requireViewable && !vw {
			continue
		}
		out = append(out, ViewableImpression{TraceID: b.TraceID, CampaignID: b.CampaignID, Timestamp: b.ObservedAt, Viewable: vw})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return out, nil
}

// InsertAttributionTouchpoints appends multi-touch chain rows (memory parity).
func (s *MemoryStore) InsertAttributionTouchpoints(_ context.Context, rows []*AttributionTouchpointRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		if r != nil {
			s.attributionTouchpoints = append(s.attributionTouchpoints, *r)
		}
	}
	return nil
}

// AttributionChain returns a conversion's chain rows, oldest first (memory parity).
func (s *MemoryStore) AttributionChain(_ context.Context, conversionTraceID string) ([]AttributionTouchpointRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []AttributionTouchpointRow
	for _, r := range s.attributionTouchpoints {
		if r.ConversionTraceID == conversionTraceID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TouchpointAt.Before(out[j].TouchpointAt) })
	return out, nil
}

// AttributionTouchpoints returns a copy of stored chain rows for test assertions.
func (s *MemoryStore) AttributionTouchpoints() []AttributionTouchpointRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AttributionTouchpointRow, len(s.attributionTouchpoints))
	copy(out, s.attributionTouchpoints)
	return out
}

// InsertProfileSignals appends the EXPANDED per-id onboarding rows (ADR 0006
// phase 1) — memory-backend parity for the ClickHouse bulk insert.
func (s *MemoryStore) InsertProfileSignals(_ context.Context, es []*ProfileSignalRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range es {
		if e == nil {
			continue
		}
		s.profileSignals = append(s.profileSignals, *e)
	}
	return nil
}

// BehaviourSignals returns a copy of all stored behaviour-signal rows for test
// assertions.
func (s *MemoryStore) BehaviourSignals() []BehaviourSignalRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]BehaviourSignalRow, len(s.behaviourSignals))
	copy(out, s.behaviourSignals)
	return out
}

// ProfileSignals returns a copy of all stored (expanded) profile-signal rows
// for test assertions.
func (s *MemoryStore) ProfileSignals() []ProfileSignalRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ProfileSignalRow, len(s.profileSignals))
	copy(out, s.profileSignals)
	return out
}

// ImpressionsByPublisher counts in-memory impressions per publisher since a
// cutoff (PublisherImpressionReader) — the memory-backend twin of the
// ClickHouse tiered-revenue-share month count.
func (s *MemoryStore) ImpressionsByPublisher(_ context.Context, since time.Time) (map[string]int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]int64{}
	for _, e := range s.impressions {
		if since.IsZero() || !e.Timestamp.Before(since) {
			if e.PublisherID != "" {
				out[e.PublisherID]++
			}
		}
	}
	return out, nil
}

// CreativeStats aggregates in-memory impressions + clicks per creative since a
// cutoff (CreativeStatAggregator).
func (s *MemoryStore) CreativeStats(_ context.Context, since time.Time) ([]CreativeStat, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	imps := map[string]int64{}
	clk := map[string]int64{}
	for _, e := range s.impressions {
		if since.IsZero() || !e.Timestamp.Before(since) {
			imps[e.CreativeID]++
		}
	}
	for _, e := range s.clicks {
		if since.IsZero() || !e.Timestamp.Before(since) {
			clk[e.CreativeID]++
		}
	}
	seen := map[string]bool{}
	var out []CreativeStat
	for id, n := range imps {
		out = append(out, CreativeStat{CreativeID: id, Impressions: n, Clicks: clk[id]})
		seen[id] = true
	}
	for id, n := range clk {
		if !seen[id] {
			out = append(out, CreativeStat{CreativeID: id, Clicks: n})
		}
	}
	return out, nil
}

// DSPCallStats aggregates the in-memory dsp_calls per (channel, endpoint)
// since a cutoff (DSPCallAggregator).
func (s *MemoryStore) DSPCallStats(_ context.Context, since time.Time) ([]DSPCallStat, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type acc struct {
		calls, bids, timeouts int64
		bidSum, latSum        float64
	}
	agg := map[[2]string]*acc{}
	for _, c := range s.dspCalls {
		if !since.IsZero() && c.Timestamp.Before(since) {
			continue
		}
		k := [2]string{c.Channel, c.DSPEndpoint}
		a := agg[k]
		if a == nil {
			a = &acc{}
			agg[k] = a
		}
		a.calls++
		a.latSum += float64(c.LatencyMs)
		if c.BidReceived {
			a.bids++
			a.bidSum += c.BidPriceUSD
		}
		if c.TimedOut {
			a.timeouts++
		}
	}
	var out []DSPCallStat
	for k, a := range agg {
		st := DSPCallStat{Channel: k[0], DSPEndpoint: k[1], TotalCalls: a.calls, TotalBids: a.bids, TotalTimeouts: a.timeouts}
		if a.bids > 0 {
			st.AvgBidUSD = a.bidSum / float64(a.bids)
		}
		if a.calls > 0 {
			st.AvgLatencyMs = a.latSum / float64(a.calls)
		}
		out = append(out, st)
	}
	return out, nil
}

// DSPCallCount returns how many DSP-call rows were recorded for an endpoint
// (optionally filtered to a channel). Test/ops helper.
func (s *MemoryStore) DSPCallCount(channel, endpoint string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, c := range s.dspCalls {
		if c.DSPEndpoint == endpoint && (channel == "" || c.Channel == channel) {
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
			"channel":      v.Channel,
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
	// sum_viewable mirrors the ClickHouse SUM(iab_viewable) base metric the
	// viewability_rate derived metric consumes (kept alongside the legacy
	// viewable_count/viewable_rate columns).
	return &QueryResult{
		Columns: []string{"count", "viewable_count", "sum_viewable", "viewable_rate", "avg_duration_ms"},
		Rows:    [][]interface{}{{count, viewableCount, viewableCount, viewableRate, avgDur}},
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

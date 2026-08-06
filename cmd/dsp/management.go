package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/lib/pq"
)

// Campaign management endpoints. These are real platform endpoints — the
// publisher simulator is the first consumer; the eventual Gateway admin UI
// and any programmatic advertiser API will use the same routes. NOT debug-
// gated: campaign CRUD is a normal customer capability, not a dev tool.
//
// TODO (production prerequisite, MUST land before public exposure):
//   - Require Authorization: Bearer <token> (or mTLS / OAuth2) on POST/
//     PATCH/DELETE. Read can stay open or be gated similarly per
//     organisational policy.
//   - Resolve the caller's identity from the token and pin tenant context
//     to accounts the caller has access to — today every write uses the
//     line item's owner account_id discovered via lookupLineItemAccount,
//     which trusts the path parameter. With auth, the lookup must
//     additionally verify the caller is authorised for that account.
//   - Per-role permissions (advertiser admin vs viewer vs platform admin)
//     once user/role tables are wired through Gateway.
//   - Audit log entry per mutation including caller identity and the
//     before/after diff (the audit_log table already exists; just needs
//     to be wired here).
//
// Endpoints:
//   GET    /v1/dsp/campaigns        → list (served by main.go, here for context)
//   POST   /v1/dsp/campaigns        → create line_item + targeting + creative
//   PATCH  /v1/dsp/campaigns/{id}   → update base_bid / daily_budget / status
//   DELETE /v1/dsp/campaigns/{id}   → soft-delete (status='archived')
//
// Every write does Postgres UPDATE/INSERT under tenant context and publishes
// adtech.cache.invalidate.campaigns so the warm cache in this pod (and any
// other pod listening) refreshes immediately.

// openManagementDB returns a writable Postgres connection for the campaign
// management endpoints. sql.Open is lazy — it parses the URL but doesn't
// dial — so we keep the handle even when the first Ping fails. Postgres
// DNS often isn't ready when the DSP pod boots (cluster-startup race);
// without this, mgmtDB stayed nil forever and every PATCH /v1/dsp/campaigns
// 503'd. Handlers still defend against a nil handle, but the handle is
// non-nil whenever the URL parses.
func openManagementDB(cfg *config.Config, log *slog.Logger) *sql.DB {
	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	if dbURL == "" {
		log.Warn("database.url not set, dsp management endpoints disabled")
		return nil
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("dsp management db open failed", "error", err)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Warn("dsp management db ping failed at boot; handlers will retry on demand", "error", err)
		// Don't close — leave the handle for on-demand reconnects.
		return db
	}
	log.Info("dsp management db connected")
	return db
}

// campaignWithSpend is the GET response shape — the cached Campaign with
// today's runtime spend counter and the advertiser's display name joined
// in. Spend lives in Redis, advertiser name in Postgres (accounts table),
// campaign config in the warm cache; we explicitly compose all three at
// response time instead of inflating the shared models.Campaign type.
type campaignWithSpend struct {
	models.Campaign
	SpentToday     float64 `json:"SpentToday"`
	AdvertiserName string  `json:"AdvertiserName,omitempty"`
}

// campaignsCollectionHandler dispatches by method on /v1/dsp/campaigns:
//
//	GET  → existing list (read from warm cache) + per-campaign spent counter
//	POST → create
//
// identity resolves this pod's (id, name) row in the dsps table — a func,
// not captured strings, so a pod that booted before the seed self-heals
// once the row exists (see newDSPIdentityResolver). Used by POST to scope
// the new advertiser account to this DSP (accounts.dsp_id) so it shows up
// in this pod's filtered campaign list and nowhere else.
func campaignsCollectionHandler(cache *warm.Cache[models.Campaign], db *sql.DB, bus events.EventBus, identity func() (string, string), budget *BudgetTracker, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			// The management list must show EVERY status (incl. draft +
			// paused + ended), so it reads from Postgres directly rather than
			// the bid cache — which now holds only LIVE campaigns (see the
			// loader's IncludeInactive note). This also means any DSP pod can
			// answer for any account (PG is shared), not just the pod whose
			// cache owns that account's DSP.
			scope := middleware.CallerScope(r)
			var all []models.Campaign
			if scope.Resolved && !scope.Platform && db != nil {
				loader := &postgres.CampaignLoader{Store: postgres.NewFromDB(db), AccountIDs: []string{scope.AccountID}, IncludeInactive: true}
				loaded, err := loader.LoadAll(r.Context())
				if err != nil {
					log.Error("management campaign list query failed", "error", err)
					http.Error(w, `{"error":"campaign list unavailable"}`, http.StatusServiceUnavailable)
					return
				}
				all = loaded
			} else {
				// Platform/admin caller (or no DB): the live bid cache is fine.
				all = cache.All()
			}
			out := make([]campaignWithSpend, len(all))
			for i, c := range all {
				out[i] = campaignWithSpend{Campaign: c, SpentToday: budget.Spend(c.ID)}
			}
			// Enrich with advertiser display names from accounts. Cheap single
			// SELECT for the (small) set of distinct advertiser IDs in this
			// pod's cache. Skip if DB unavailable — the UI still works without
			// names, just shows shortened UUIDs.
			if db != nil {
				if names, err := lookupAccountNames(r.Context(), db, distinctAdvertiserIDs(all)); err == nil {
					for i := range out {
						out[i].AdvertiserName = names[out[i].AdvertiserID]
					}
				} else {
					log.Warn("advertiser name lookup failed", "error", err)
				}
			}
			json.NewEncoder(w).Encode(out)
		case http.MethodPost:
			if db == nil {
				http.Error(w, "management db unavailable", http.StatusServiceUnavailable)
				return
			}
			dspID, dspName := identity()
			if dspID == "" {
				http.Error(w, "this dsp has no dsps row (migration 022 + seed required); cannot create campaigns", http.StatusBadRequest)
				return
			}
			handleCreate(w, r, db, bus, dspID, dspName, log)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// campaignByIDHandler matches /v1/dsp/campaigns/{id} and dispatches by method.
// http.ServeMux without trailing-slash routing means we register the prefix
// and parse the ID from the path tail.
func campaignByIDHandler(db *sql.DB, bus events.EventBus, accountIDs []string, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, "management db unavailable", http.StatusServiceUnavailable)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/dsp/campaigns/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPatch:
			handlePatch(w, r, db, bus, id, log)
		case http.MethodDelete:
			handleDelete(w, r, db, bus, id, log)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// validBidStrategies / validPacingModes mirror the line_items CHECK
// constraints (migration 005). Kept here so the API rejects bad values with
// a clear 400 instead of leaking a raw Postgres constraint error.
var validBidStrategies = map[string]bool{"cpm": true, "cpc": true, "cpa": true, "vcpm": true, "cpcv": true}
var validCampaignFormats = map[string]bool{"display": true, "native": true, "video": true, "audio": true}
var validPacingModes = map[string]bool{"even": true, "asap": true, "front_loaded": true}
var validCreativeRotations = map[string]bool{"even": true, "weighted": true, "bandit": true, "sequential": true}

// uuidRe validates a creative_id before it reaches a ::uuid[] cast (a clear
// 400 beats a raw Postgres cast error).
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// creativeAttach is one creative attached to a campaign with a rotation weight.
// The DSP picks the highest-weight size-matched creative at bid time; finer
// rotation (bandit/sequential) is the ad server's job.
type creativeAttach struct {
	CreativeID string `json:"creative_id"`
	Weight     int    `json:"weight"`
}

// bidModifiersInput is the structured bid-modifier map stored in
// targeting_rules.bid_modifiers (JSONB) and read back by the CampaignLoader's
// parseModifiers. Keys are device names / country codes; values are percentage
// adjustments (+20 = bid 20% higher). The engine clamps each to safe bounds.
type bidModifiersInput struct {
	Device     map[string]float64 `json:"device,omitempty"`
	GeoCountry map[string]float64 `json:"geo_country,omitempty"`
	// Audience maps segment id → percentage adjustment, applied when the
	// bid request's (consent-gated) segment set contains the key. The
	// profile store's pre-expanded memberships are what make this fire for
	// every device of an enrolled person.
	Audience  map[string]float64  `json:"audience,omitempty"`
	TimeOfDay []timeModifierInput `json:"time_of_day,omitempty"`
}

// timeModifierInput is one time-window bid adjustment. The hour window is
// [StartHour, EndHour); EndHour <= StartHour wraps past midnight. Evaluated in
// the campaign's timezone (line_items.timezone).
type timeModifierInput struct {
	StartHour int     `json:"start_hour"`
	EndHour   int     `json:"end_hour"`
	Modifier  float64 `json:"modifier"`
}

// validateAndJSON rejects wildly out-of-range percentages (the engine clamps
// too, but a clear 400 beats a silently-clamped surprise) and marshals to the
// JSONB the column stores. A nil receiver yields "{}".
func (b *bidModifiersInput) validateAndJSON() (string, error) {
	if b == nil {
		return "{}", nil
	}
	for _, m := range []map[string]float64{b.Device, b.GeoCountry, b.Audience} {
		for k, v := range m {
			if v < -100 || v > 1000 {
				return "", fmt.Errorf("bid modifier %q = %v out of range (-100..1000)", k, v)
			}
		}
	}
	for i, tm := range b.TimeOfDay {
		if tm.Modifier < -100 || tm.Modifier > 1000 {
			return "", fmt.Errorf("time_of_day[%d] modifier %v out of range (-100..1000)", i, tm.Modifier)
		}
		if tm.StartHour < 0 || tm.StartHour > 23 || tm.EndHour < 0 || tm.EndHour > 24 {
			return "", fmt.Errorf("time_of_day[%d] hours must be 0-23 (start) / 0-24 (end)", i)
		}
	}
	j, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	return string(j), nil
}

// freqCapInput is the advertiser-configured per-campaign frequency cap. It is
// stored in targeting_rules.frequency_caps as the line_item-dimension entry and
// enforced by the ad server (per-user impression counter). Limit <= 0 clears
// the cap (falls back to the platform default).
type freqCapInput struct {
	Limit  int    `json:"limit"`
	Window string `json:"window,omitempty"` // hour | day | week; default day
}

// validateAndJSON validates the cap and marshals it to the frequency_caps JSONB
// array shape. A nil/zero cap yields "[]" (no per-campaign cap).
func (f *freqCapInput) validateAndJSON() (string, error) {
	if f == nil || f.Limit <= 0 {
		return "[]", nil
	}
	if f.Limit > 100000 {
		return "", fmt.Errorf("frequency cap limit %d too large", f.Limit)
	}
	win := f.Window
	if win == "" {
		win = "day"
	}
	switch win {
	case "hour", "day", "week":
	default:
		return "", fmt.Errorf("frequency cap window must be hour, day or week")
	}
	b, err := json.Marshal([]map[string]any{
		{"dimension": "line_item", "window": win, "limit": f.Limit},
	})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type createCampaignRequest struct {
	Name        string  `json:"name"`
	BaseBid     float64 `json:"base_bid"`
	DailyBudget float64 `json:"daily_budget"`
	// Format is the line item's ad format (display|native|video|audio). Empty →
	// display. A display campaign gets an auto-generated placeholder banner; a
	// non-display campaign starts with no creative — attach one via PATCH
	// `creatives` (only approved, format-matching creatives serve).
	Format string `json:"format,omitempty"`
	// ProductCategory is the advertised product's own IAB category (retail
	// relevance signal), distinct from include_categories (content targeting).
	ProductCategory string   `json:"product_category,omitempty"`
	IncludeGeo      []string `json:"include_geo,omitempty"`
	ExcludeGeo      []string `json:"exclude_geo,omitempty"`
	IncludeDevice   []string `json:"include_device,omitempty"`
	ExcludeDevice   []string `json:"exclude_device,omitempty"`
	// Domain / category targeting — the DSP targeting engine evaluates these
	// against the bid request. Empty list = no constraint on that dimension.
	IncludeDomains    []string `json:"include_domains,omitempty"`
	ExcludeDomains    []string `json:"exclude_domains,omitempty"`
	IncludeCategories []string `json:"include_categories,omitempty"`
	ExcludeCategories []string `json:"exclude_categories,omitempty"`
	// OS / keyword / inventory-type targeting — also evaluated by the DSP
	// engine. OS matches Device.os; keywords match Site.keywords (page
	// keywords); inventory type is "site" or "app".
	IncludeOS            []string `json:"include_os,omitempty"`
	IncludeKeywords      []string `json:"include_keywords,omitempty"`
	ExcludeKeywords      []string `json:"exclude_keywords,omitempty"`
	IncludeInventoryType []string `json:"include_inventory_type,omitempty"`
	// Channel allowlist — which channels (display/video/audio/native/dooh/retail/
	// ingame) this campaign is eligible for. Empty = all channels.
	IncludeChannels []string `json:"include_channels,omitempty"`
	// Audience segment targeting — matched against the user's public segments
	// (SSP-stamped) unioned with the DSP's private segments.
	IncludeSegments []string `json:"include_segments,omitempty"`
	ExcludeSegments []string `json:"exclude_segments,omitempty"`
	// BidModifiers adjust the bid by percentage per dimension (device/geo);
	// applied by the DSP before the floor check. nil = no modifiers.
	BidModifiers *bidModifiersInput `json:"bid_modifiers,omitempty"`
	// FreqCap is the advertiser's per-user impression cap for this campaign,
	// enforced by the ad server. nil = platform-default cap.
	FreqCap *freqCapInput `json:"frequency_cap,omitempty"`
	// BidStrategy selects the billing model: cpm (default) bills on
	// impression; cpc/cpa/vcpm/cpcv reserve on impression and settle on the
	// trigger event (click/conversion/viewable/complete). Empty → cpm.
	BidStrategy string `json:"bid_strategy,omitempty"`
	// PacingMode spreads the daily budget: even (default), asap, front_loaded.
	PacingMode string `json:"pacing_mode,omitempty"`
	// TotalBudget is the campaign-level (IO) budget cap. Zero → daily×30.
	TotalBudget float64 `json:"total_budget,omitempty"`
	// Flight window (IO start/end). Empty → today .. +90d. Format YYYY-MM-DD.
	StartDate string `json:"start_date,omitempty"`
	EndDate   string `json:"end_date,omitempty"`
	// Timezone (IANA) for time-of-day bid modifiers. Empty → UTC.
	Timezone string `json:"timezone,omitempty"`
	// CreativeRotation is the multi-creative rotation mode (even|weighted|
	// bandit|sequential). Empty → bandit.
	CreativeRotation string `json:"creative_rotation,omitempty"`
	// ViewabilityTargetPct is the contractual viewability guarantee
	// (0-100). Optional — nil = no guarantee, no makegood reconciliation.
	// Settlement mechanics for vCPM are a separate platform-level decision
	// (see docs/PLAN.md → "vCPM Settlement Model").
	ViewabilityTargetPct *int `json:"viewability_target_pct,omitempty"`
}

func handleCreate(w http.ResponseWriter, r *http.Request, db *sql.DB, bus events.EventBus, dspID, dspName string, log *slog.Logger) {
	var req createCampaignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if req.BaseBid <= 0 {
		req.BaseBid = 2.50
	}
	if req.DailyBudget <= 0 {
		req.DailyBudget = 500
	}
	if req.BidStrategy == "" {
		req.BidStrategy = "cpm"
	}
	if !validBidStrategies[req.BidStrategy] {
		http.Error(w, "bid_strategy must be cpm, cpc, cpa, vcpm or cpcv", http.StatusBadRequest)
		return
	}
	if req.Format == "" {
		req.Format = "display"
	}
	if !validCampaignFormats[req.Format] {
		http.Error(w, "format must be display, native, video or audio", http.StatusBadRequest)
		return
	}
	if req.PacingMode == "" {
		req.PacingMode = "even"
	}
	if !validPacingModes[req.PacingMode] {
		http.Error(w, "pacing_mode must be even, asap or front_loaded", http.StatusBadRequest)
		return
	}
	if req.StartDate != "" || req.EndDate != "" {
		if !validDate(req.StartDate) || !validDate(req.EndDate) {
			http.Error(w, "start_date and end_date must both be YYYY-MM-DD when set", http.StatusBadRequest)
			return
		}
		if req.EndDate < req.StartDate {
			http.Error(w, "end_date must be on or after start_date", http.StatusBadRequest)
			return
		}
	}
	if req.ViewabilityTargetPct != nil {
		v := *req.ViewabilityTargetPct
		if v < 0 || v > 100 {
			http.Error(w, "viewability_target_pct must be 0-100", http.StatusBadRequest)
			return
		}
	}
	if _, err := req.BidModifiers.validateAndJSON(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := req.FreqCap.validateAndJSON(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(req.Timezone); err != nil {
		http.Error(w, "invalid timezone: "+req.Timezone, http.StatusBadRequest)
		return
	}
	if req.CreativeRotation == "" {
		req.CreativeRotation = "bandit"
	}
	if !validCreativeRotations[req.CreativeRotation] {
		http.Error(w, "creative_rotation must be even, weighted, bandit or sequential", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Whose campaign is this? A customer session (advertiser via the
	// gateway) creates under its own account — anything else and the tenant
	// read filter above would hide their own campaign from them. Platform
	// callers (pub sim, e2e, operator key) use the DSP's "default management
	// advertiser" bucket, as before.
	scope := middleware.CallerScope(r)
	var accountID string
	var err error
	if scope.Resolved && !scope.Platform {
		accountID = scope.AccountID
		// Attach the account to this DSP on first use (the campaign warm
		// cache filters by accounts.dsp_id). COALESCE keeps an existing
		// attachment — an account already on another DSP isn't stolen.
		if _, err := db.ExecContext(ctx,
			`UPDATE accounts SET dsp_id = COALESCE(dsp_id, $1::uuid), updated_at = now() WHERE id = $2::uuid`,
			dspID, accountID); err != nil {
			log.Error("attach account to dsp failed", "error", err, "account_id", accountID)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	} else {
		accountID, err = ensureMgmtAdvertiser(ctx, db, dspID, dspName)
		if err != nil {
			log.Error("ensure mgmt advertiser failed", "error", err, "dsp", dspName)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Mirror the e2e harness CreateCampaign pattern: line_item + targeting +
	// creative + link, all under one tenant context so RLS admits the inserts.
	// External key includes a timestamp suffix so repeated "+ New Campaign"
	// clicks with the same name don't collide on the deterministic UUID.
	suffix := fmt.Sprintf("%s-%d", req.Name, time.Now().UnixNano())
	lineItemID := idgen.Derive("line_item", suffix)
	creativeID := idgen.Derive("creative", suffix)
	targetingID := idgen.Derive("targeting", suffix)
	ioID := idgen.Derive("io", "mgmt-"+suffix)

	if err := writeNewCampaign(ctx, db, accountID, ioID, lineItemID, creativeID, targetingID, req); err != nil {
		log.Error("create campaign failed", "error", err, "name", req.Name)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	publishInvalidate(ctx, bus, log, "create", lineItemID)

	// Record who created what (scope resolved above — customer sessions
	// created under their own account, platform callers under the bucket).
	if err := audit.Log(ctx, db, audit.Entry{
		AccountID: accountID, ActorID: scope.Actor, Action: "campaign:create",
		ResourceType: "line_item", ResourceID: lineItemID, Changes: req,
	}); err != nil {
		log.Warn("audit log write failed", "action", "campaign:create", "id", lineItemID, "error", err)
	}

	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	// Return account_id so clients can act on the owning account without a
	// follow-up list read (which races the warm-cache invalidate).
	json.NewEncoder(w).Encode(map[string]string{"id": lineItemID, "account_id": accountID, "status": "created"})
}

// ensureMgmtAdvertiser returns the UUID of the per-DSP "default management
// advertiser" account, creating it on first call. UUID is deterministic so
// every call from the same DSP returns the same account → all UI-created
// campaigns for that DSP share an advertiser bucket. dsp_id is set so the
// CampaignLoader's WHERE acc.dsp_id = ? filter includes it.
func ensureMgmtAdvertiser(ctx context.Context, db *sql.DB, dspID, dspName string) (string, error) {
	externalKey := dspName + "-mgmt"
	accountID := idgen.Derive("account", externalKey)
	const q = `
INSERT INTO accounts (id, name, email, type, currency, status, dsp_id, created_at, updated_at)
VALUES ($1, $2, $3, 'advertiser', 'USD', 'active', $4::uuid, now(), now())
ON CONFLICT (id) DO UPDATE SET dsp_id = EXCLUDED.dsp_id, updated_at = now()`
	if _, err := db.ExecContext(ctx, q,
		accountID,
		dspName+" — UI-created campaigns",
		externalKey+"@mgmt.local",
		dspID,
	); err != nil {
		return "", fmt.Errorf("upsert mgmt advertiser: %w", err)
	}
	return accountID, nil
}

// writeNewCampaign inserts the four rows needed for a bid-eligible campaign:
// insertion_orders, line_items, targeting_rules, creatives, line_item_creatives.
// All under SET LOCAL tenant context so the RLS policies admit the writes.
func writeNewCampaign(ctx context.Context, db *sql.DB, accountID, ioID, lineItemID, creativeID, targetingID string, req createCampaignRequest) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_account_id', $1, true)", accountID); err != nil {
		return fmt.Errorf("set tenant: %w", err)
	}

	// IO (parent budget container). Total budget defaults to daily×30;
	// flight window defaults to today .. +90d. Empty date strings fall back
	// to those SQL defaults via COALESCE on a NULL cast.
	totalBudget := req.TotalBudget
	if totalBudget <= 0 {
		totalBudget = req.DailyBudget * 30
	}
	var startArg, endArg any
	if req.StartDate != "" {
		startArg = req.StartDate
	}
	if req.EndDate != "" {
		endArg = req.EndDate
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO insertion_orders (id, account_id, name, budget, daily_budget, currency, start_date, end_date, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, 'USD',
        COALESCE($6::date, current_date),
        COALESCE($7::date, current_date + interval '90 days'),
        'active', now(), now())
ON CONFLICT (id) DO NOTHING`, ioID, accountID, "mgmt-"+req.Name, totalBudget, req.DailyBudget, startArg, endArg); err != nil {
		return fmt.Errorf("io insert: %w", err)
	}
	// Line item (the "campaign" in our parlance). bid_strategy + pacing_mode
	// now come from the request (validated by the caller). viewability_target_pct
	// is NULL when omitted.
	var viewTarget any
	if req.ViewabilityTargetPct != nil {
		viewTarget = *req.ViewabilityTargetPct
	}
	// A campaign must not go LIVE with nothing to serve — the DSP would
	// silently skip it at bid time (no matching creative). Display gets an
	// auto-placeholder below so it can serve immediately; non-display
	// (video/native/audio) starts creative-less, so it opens as 'draft'
	// (excluded from bidding) until the advertiser attaches a real creative,
	// which auto-promotes it to live (see replaceLineItemCreatives).
	initialStatus := "live"
	if req.Format != "display" {
		initialStatus = "draft"
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO line_items (id, account_id, insertion_order_id, name, status, format, bid_strategy, base_bid, bid_currency, daily_budget, pacing_mode, shading_mode, creative_rotation, timezone, viewability_target_pct, product_category, created_at, updated_at)
VALUES ($1, $2, $3, $4, $13, $5, $6, $7, 'USD', $8, $9, 'moderate', $10, $11, $12, NULLIF($14, ''), now(), now())`,
		lineItemID, accountID, ioID, req.Name, req.Format, req.BidStrategy, req.BaseBid, req.DailyBudget, req.PacingMode, req.CreativeRotation, req.Timezone, viewTarget, initialStatus, req.ProductCategory); err != nil {
		return fmt.Errorf("line_item insert: %w", err)
	}
	// Targeting — geo/device/domain/category include+exclude. The DSP
	// engine treats an empty array as "no constraint on that dimension".
	modifiersJSON, _ := req.BidModifiers.validateAndJSON() // already validated in handleCreate
	freqCapsJSON, _ := req.FreqCap.validateAndJSON()       // already validated in handleCreate
	if _, err := tx.ExecContext(ctx, `
INSERT INTO targeting_rules (id, line_item_id, account_id,
    include_geo, exclude_geo, include_device, exclude_device,
    include_domains, exclude_domains, include_categories, exclude_categories,
    include_os, include_keywords, exclude_keywords, include_inventory_type,
    include_segments, exclude_segments, include_channels,
    bid_modifiers, frequency_caps, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19::jsonb, $20::jsonb, now(), now())`,
		targetingID, lineItemID, accountID,
		pq.StringArray(req.IncludeGeo), pq.StringArray(req.ExcludeGeo),
		pq.StringArray(req.IncludeDevice), pq.StringArray(req.ExcludeDevice),
		pq.StringArray(req.IncludeDomains), pq.StringArray(req.ExcludeDomains),
		pq.StringArray(req.IncludeCategories), pq.StringArray(req.ExcludeCategories),
		pq.StringArray(req.IncludeOS), pq.StringArray(req.IncludeKeywords),
		pq.StringArray(req.ExcludeKeywords), pq.StringArray(req.IncludeInventoryType),
		pq.StringArray(req.IncludeSegments), pq.StringArray(req.ExcludeSegments),
		pq.StringArray(req.IncludeChannels),
		modifiersJSON, freqCapsJSON); err != nil {
		return fmt.Errorf("targeting insert: %w", err)
	}
	// Auto-generate a placeholder banner only for DISPLAY campaigns so the
	// campaign can serve immediately. This is SYSTEM content (a gradient +
	// the campaign name, hardcoded below — never advertiser-supplied), so
	// auto-approving it is not a review bypass: real advertiser creatives
	// always go through pending_review via the creative-upload path. The
	// name is suffixed "(auto placeholder)" so it's obvious in the creative
	// list this is a stand-in to replace with a real, reviewed creative.
	if req.Format == "display" {
		html := fmt.Sprintf(`<div style="width:${WIDTH}px;height:${HEIGHT}px;background:linear-gradient(135deg,#4ECDC4,#556270);color:white;display:flex;align-items:center;justify-content:center;font-family:sans-serif;text-align:center;padding:8px;box-sizing:border-box;border-radius:4px;"><div><strong>%s</strong><br><small>via mgmt</small></div></div>`, req.Name)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO creatives (id, account_id, name, format, width, height, landing_url, html_content, review_status, created_at, updated_at)
VALUES ($1, $2, $3, 'display', 300, 250, $4, $5, 'approved', now(), now())`,
			creativeID, accountID, req.Name+" (auto placeholder)", "https://"+strings.ToLower(strings.ReplaceAll(req.Name, " ", "-"))+".test", html); err != nil {
			return fmt.Errorf("creative insert: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO line_item_creatives (line_item_id, creative_id, weight) VALUES ($1, $2, 100)`,
			lineItemID, creativeID); err != nil {
			return fmt.Errorf("line_item_creatives insert: %w", err)
		}
	}
	return tx.Commit()
}

type patchCampaignRequest struct {
	BaseBid     *float64 `json:"base_bid,omitempty"`
	DailyBudget *float64 `json:"daily_budget,omitempty"`
	Status      *string  `json:"status,omitempty"`
	BidStrategy *string  `json:"bid_strategy,omitempty"`
	PacingMode  *string  `json:"pacing_mode,omitempty"`
	Timezone    *string  `json:"timezone,omitempty"`
	// ProductCategory replaces line_items.product_category (retail relevance).
	ProductCategory *string `json:"product_category,omitempty"`
	// CreativeRotation changes the rotation mode; Creatives replaces the
	// campaign's attached creatives (line_item_creatives) with the given set.
	CreativeRotation *string           `json:"creative_rotation,omitempty"`
	Creatives        *[]creativeAttach `json:"creatives,omitempty"`
	// Targeting edits — nil leaves the column unchanged; a supplied list
	// (even empty) replaces it. Lands in targeting_rules, same tx.
	IncludeGeo           *[]string `json:"include_geo,omitempty"`
	ExcludeGeo           *[]string `json:"exclude_geo,omitempty"`
	IncludeDevice        *[]string `json:"include_device,omitempty"`
	ExcludeDevice        *[]string `json:"exclude_device,omitempty"`
	IncludeDomains       *[]string `json:"include_domains,omitempty"`
	ExcludeDomains       *[]string `json:"exclude_domains,omitempty"`
	IncludeCategories    *[]string `json:"include_categories,omitempty"`
	ExcludeCategories    *[]string `json:"exclude_categories,omitempty"`
	IncludeOS            *[]string `json:"include_os,omitempty"`
	IncludeKeywords      *[]string `json:"include_keywords,omitempty"`
	ExcludeKeywords      *[]string `json:"exclude_keywords,omitempty"`
	IncludeInventoryType *[]string `json:"include_inventory_type,omitempty"`
	IncludeSegments      *[]string `json:"include_segments,omitempty"`
	ExcludeSegments      *[]string `json:"exclude_segments,omitempty"`
	IncludeChannels      *[]string `json:"include_channels,omitempty"`
	// BidModifiers replaces the whole bid_modifiers JSONB when supplied.
	BidModifiers *bidModifiersInput `json:"bid_modifiers,omitempty"`
	// FreqCap replaces the frequency_caps JSONB when supplied (limit <= 0 clears).
	FreqCap *freqCapInput `json:"frequency_cap,omitempty"`
}

// hasTargeting reports whether the patch touches any targeting column.
func (p patchCampaignRequest) hasTargeting() bool {
	return p.IncludeGeo != nil || p.ExcludeGeo != nil ||
		p.IncludeDevice != nil || p.ExcludeDevice != nil ||
		p.IncludeDomains != nil || p.ExcludeDomains != nil ||
		p.IncludeCategories != nil || p.ExcludeCategories != nil ||
		p.IncludeOS != nil || p.IncludeKeywords != nil ||
		p.ExcludeKeywords != nil || p.IncludeInventoryType != nil ||
		p.IncludeSegments != nil || p.ExcludeSegments != nil ||
		p.IncludeChannels != nil ||
		p.BidModifiers != nil || p.FreqCap != nil
}

// validDate reports whether s is a YYYY-MM-DD date (or empty).
func validDate(s string) bool {
	if s == "" {
		return true
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func handlePatch(w http.ResponseWriter, r *http.Request, db *sql.DB, bus events.EventBus, id string, log *slog.Logger) {
	var req patchCampaignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.BaseBid == nil && req.DailyBudget == nil && req.Status == nil &&
		req.BidStrategy == nil && req.PacingMode == nil && req.Timezone == nil &&
		req.CreativeRotation == nil && req.Creatives == nil && req.ProductCategory == nil && !req.hasTargeting() {
		http.Error(w, "no fields to update", http.StatusBadRequest)
		return
	}
	if req.CreativeRotation != nil && !validCreativeRotations[*req.CreativeRotation] {
		http.Error(w, "creative_rotation must be even, weighted, bandit or sequential", http.StatusBadRequest)
		return
	}
	if req.Creatives != nil {
		if len(*req.Creatives) == 0 {
			http.Error(w, "creatives must list at least one creative", http.StatusBadRequest)
			return
		}
		for _, cr := range *req.Creatives {
			if !uuidRe.MatchString(cr.CreativeID) {
				http.Error(w, "creatives[].creative_id must be a UUID", http.StatusBadRequest)
				return
			}
			if cr.Weight < 0 {
				http.Error(w, "creatives[].weight must be >= 0", http.StatusBadRequest)
				return
			}
		}
	}
	if req.Timezone != nil {
		if _, err := time.LoadLocation(*req.Timezone); err != nil {
			http.Error(w, "invalid timezone: "+*req.Timezone, http.StatusBadRequest)
			return
		}
	}
	if req.Status != nil {
		switch *req.Status {
		case "live", "paused", "archived", "ended":
		default:
			http.Error(w, "invalid status", http.StatusBadRequest)
			return
		}
	}
	if req.BidStrategy != nil && !validBidStrategies[*req.BidStrategy] {
		http.Error(w, "bid_strategy must be cpm, cpc, cpa, vcpm or cpcv", http.StatusBadRequest)
		return
	}
	if req.PacingMode != nil && !validPacingModes[*req.PacingMode] {
		http.Error(w, "pacing_mode must be even, asap or front_loaded", http.StatusBadRequest)
		return
	}
	if _, err := req.BidModifiers.validateAndJSON(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := req.FreqCap.validateAndJSON(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	accountID, oldStatus, err := lookupLineItemAccountAndStatus(ctx, db, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// Tenant isolation: an account-scoped caller may only mutate its own
	// account's campaigns; a platform operator key may mutate any. Closes
	// cross-tenant mutation (the IDOR the API-key-only gate left open).
	scope := middleware.CallerScope(r)
	if !scope.CanMutate(accountID) {
		log.Warn("patch campaign forbidden: caller not authorised for account", "actor", scope.Actor, "target_account", accountID, "id", id)
		http.Error(w, "forbidden: not authorised for this account", http.StatusForbidden)
		return
	}

	if err := updateLineItem(ctx, db, accountID, id, req); err != nil {
		if errors.Is(err, errCreativeNotAttachable) || errors.Is(err, errNoEligibleCreative) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Error("patch campaign failed", "error", err, "id", id)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	publishInvalidate(ctx, bus, log, "patch", id)
	if req.Status != nil {
		publishCampaignStateChange(ctx, bus, log, id, accountID, oldStatus, *req.Status, "patch")
	}
	if err := audit.Log(ctx, db, audit.Entry{
		AccountID: accountID, ActorID: scope.Actor, Action: "campaign:update",
		ResourceType: "line_item", ResourceID: id, Changes: req,
	}); err != nil {
		log.Warn("audit log write failed", "action", "campaign:update", "id", id, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func updateLineItem(ctx context.Context, db *sql.DB, accountID, lineItemID string, req patchCampaignRequest) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_account_id', $1, true)", accountID); err != nil {
		return err
	}
	// Build dynamic SET clause from supplied fields. Could use squirrel here
	// but for three fields the hand-rolled approach stays readable.
	sets := []string{"updated_at = now()"}
	args := []any{}
	if req.BaseBid != nil {
		args = append(args, *req.BaseBid)
		sets = append(sets, fmt.Sprintf("base_bid = $%d", len(args)))
	}
	if req.DailyBudget != nil {
		args = append(args, *req.DailyBudget)
		sets = append(sets, fmt.Sprintf("daily_budget = $%d", len(args)))
	}
	if req.Status != nil {
		args = append(args, *req.Status)
		sets = append(sets, fmt.Sprintf("status = $%d", len(args)))
	}
	if req.BidStrategy != nil {
		args = append(args, *req.BidStrategy)
		sets = append(sets, fmt.Sprintf("bid_strategy = $%d", len(args)))
	}
	if req.PacingMode != nil {
		args = append(args, *req.PacingMode)
		sets = append(sets, fmt.Sprintf("pacing_mode = $%d", len(args)))
	}
	if req.Timezone != nil {
		args = append(args, *req.Timezone)
		sets = append(sets, fmt.Sprintf("timezone = $%d", len(args)))
	}
	if req.CreativeRotation != nil {
		args = append(args, *req.CreativeRotation)
		sets = append(sets, fmt.Sprintf("creative_rotation = $%d", len(args)))
	}
	if req.ProductCategory != nil {
		// Empty string clears it — the loader COALESCEs NULL/'' the same and the
		// DSP treats '' as unset (falls back to include_categories).
		args = append(args, *req.ProductCategory)
		sets = append(sets, fmt.Sprintf("product_category = NULLIF($%d, '')", len(args)))
	}
	args = append(args, lineItemID)
	q := fmt.Sprintf("UPDATE line_items SET %s WHERE id = $%d", strings.Join(sets, ", "), len(args))
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("campaign not found or RLS blocked update")
	}
	// Targeting edits land in the same tx on targeting_rules (keyed by
	// line_item_id) so a campaign + targeting patch is atomic.
	if req.hasTargeting() {
		if err := updateTargeting(ctx, tx, lineItemID, req); err != nil {
			return err
		}
	}
	// Creative attachments replace line_item_creatives when supplied.
	if req.Creatives != nil {
		if err := replaceLineItemCreatives(ctx, tx, accountID, lineItemID, *req.Creatives); err != nil {
			return err
		}
		// A campaign parked as 'draft' for want of a creative goes live the
		// moment it has one — the counterpart to the create-time guard. Only
		// touches 'draft' (never resurrects a paused/ended campaign), and
		// only when at least one creative is now attached.
		if len(*req.Creatives) > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE line_items SET status='live', updated_at=now() WHERE id=$1 AND account_id=$2 AND status='draft'`,
				lineItemID, accountID); err != nil {
				return fmt.Errorf("promote draft campaign: %w", err)
			}
		}
	}
	// A campaign must not be activated with nothing to serve: setting status
	// to 'live' (un-pausing, or promoting a draft directly) requires at least
	// one ATTACHED, APPROVED creative — otherwise the DSP silently skips it at
	// bid time. Counts creatives attached earlier in THIS tx, so a
	// status=live + creatives=[...] patch that attaches an approved creative is
	// allowed. Only fires on the transition to live; unrelated edits to an
	// already-live campaign are untouched.
	if req.Status != nil && *req.Status == "live" {
		eligible, err := countEligibleCreatives(ctx, tx, accountID, lineItemID)
		if err != nil {
			return err
		}
		if eligible == 0 {
			return errNoEligibleCreative
		}
	}
	return tx.Commit()
}

// errNoEligibleCreative → 400: a campaign was asked to go live but has no
// attached, approved creative to serve.
var errNoEligibleCreative = errors.New("cannot activate campaign: attach at least one approved creative first")

// countEligibleCreatives returns how many of a campaign's attached creatives
// are approved — the exact eligibility the bid path serves on (only approved,
// format-matching creatives serve). Runs in updateLineItem's tx so it sees
// creatives attached earlier in the same request.
func countEligibleCreatives(ctx context.Context, tx *sql.Tx, accountID, lineItemID string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM line_item_creatives lic
		 JOIN creatives c ON c.id = lic.creative_id
		 WHERE lic.line_item_id = $1::uuid AND c.account_id = $2::uuid AND c.review_status = 'approved'`,
		lineItemID, accountID).Scan(&n)
	return n, err
}

// errCreativeNotAttachable → 400: an attached creative isn't owned by the
// account or isn't approved.
var errCreativeNotAttachable = errors.New("creative not owned by account or not approved")

// replaceLineItemCreatives swaps the campaign's attached creatives for the
// supplied set. Every creative must belong to accountID and be approved
// (only approved creatives are allowed to serve). Runs in updateLineItem's tx.
func replaceLineItemCreatives(ctx context.Context, tx *sql.Tx, accountID, lineItemID string, attach []creativeAttach) error {
	ids := make([]string, len(attach))
	for i, a := range attach {
		ids[i] = a.CreativeID
	}
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM creatives
		 WHERE id = ANY($1::uuid[]) AND account_id = $2::uuid AND review_status = 'approved'`,
		pq.Array(ids), accountID).Scan(&n); err != nil {
		return err
	}
	if n != len(ids) {
		return errCreativeNotAttachable
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM line_item_creatives WHERE line_item_id = $1::uuid`, lineItemID); err != nil {
		return err
	}
	for _, a := range attach {
		w := a.Weight
		if w <= 0 {
			w = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO line_item_creatives (line_item_id, creative_id, weight) VALUES ($1::uuid, $2::uuid, $3)`,
			lineItemID, a.CreativeID, w); err != nil {
			return err
		}
	}
	return nil
}

// updateTargeting applies the supplied targeting arrays to the line item's
// targeting_rules row (only the columns present in the patch). Runs inside
// updateLineItem's tenant tx.
func updateTargeting(ctx context.Context, tx *sql.Tx, lineItemID string, req patchCampaignRequest) error {
	sets := []string{"updated_at = now()"}
	args := []any{}
	add := func(col string, v *[]string) {
		if v != nil {
			args = append(args, pq.StringArray(*v))
			sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
		}
	}
	add("include_geo", req.IncludeGeo)
	add("exclude_geo", req.ExcludeGeo)
	add("include_device", req.IncludeDevice)
	add("exclude_device", req.ExcludeDevice)
	add("include_domains", req.IncludeDomains)
	add("exclude_domains", req.ExcludeDomains)
	add("include_categories", req.IncludeCategories)
	add("exclude_categories", req.ExcludeCategories)
	add("include_os", req.IncludeOS)
	add("include_keywords", req.IncludeKeywords)
	add("exclude_keywords", req.ExcludeKeywords)
	add("include_inventory_type", req.IncludeInventoryType)
	add("include_segments", req.IncludeSegments)
	add("exclude_segments", req.ExcludeSegments)
	add("include_channels", req.IncludeChannels)
	// bid_modifiers / frequency_caps are JSONB, not TEXT[] — handle separately.
	if req.BidModifiers != nil {
		j, err := req.BidModifiers.validateAndJSON()
		if err != nil {
			return fmt.Errorf("bid_modifiers: %w", err)
		}
		args = append(args, j)
		sets = append(sets, fmt.Sprintf("bid_modifiers = $%d::jsonb", len(args)))
	}
	if req.FreqCap != nil {
		j, err := req.FreqCap.validateAndJSON()
		if err != nil {
			return fmt.Errorf("frequency_cap: %w", err)
		}
		args = append(args, j)
		sets = append(sets, fmt.Sprintf("frequency_caps = $%d::jsonb", len(args)))
	}
	args = append(args, lineItemID)
	q := fmt.Sprintf("UPDATE targeting_rules SET %s WHERE line_item_id = $%d", strings.Join(sets, ", "), len(args))
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("targeting update: %w", err)
	}
	return nil
}

func handleDelete(w http.ResponseWriter, r *http.Request, db *sql.DB, bus events.EventBus, id string, log *slog.Logger) {
	// Soft delete: set status='archived'. Keeps history intact for the
	// reporting / billing surface and avoids the FK cascade (line_item_creatives,
	// targeting_rules, budget_reservations, ledger_entries…) that hard-delete
	// would require. The warm cache filters status != 'live' at bid time so
	// archived campaigns naturally disappear from the auction.
	archived := "archived"
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	accountID, oldStatus, err := lookupLineItemAccountAndStatus(ctx, db, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	scope := middleware.CallerScope(r)
	if !scope.CanMutate(accountID) {
		log.Warn("delete campaign forbidden: caller not authorised for account", "actor", scope.Actor, "target_account", accountID, "id", id)
		http.Error(w, "forbidden: not authorised for this account", http.StatusForbidden)
		return
	}
	if err := updateLineItem(ctx, db, accountID, id, patchCampaignRequest{Status: &archived}); err != nil {
		log.Error("delete (archive) campaign failed", "error", err, "id", id)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	publishInvalidate(ctx, bus, log, "delete", id)
	publishCampaignStateChange(ctx, bus, log, id, accountID, oldStatus, archived, "delete")
	if err := audit.Log(ctx, db, audit.Entry{
		AccountID: accountID, ActorID: scope.Actor, Action: "campaign:delete",
		ResourceType: "line_item", ResourceID: id, Reason: "archive",
	}); err != nil {
		log.Warn("audit log write failed", "action", "campaign:delete", "id", id, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// distinctAdvertiserIDs returns the unique advertiser_id set across a list
// of campaigns. Used to batch the account-name lookup into one SQL.
func distinctAdvertiserIDs(campaigns []models.Campaign) []string {
	seen := map[string]struct{}{}
	for _, c := range campaigns {
		if c.AdvertiserID != "" {
			seen[c.AdvertiserID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	return ids
}

// lookupAccountNames returns account_id → name. Empty input returns empty
// map without hitting the DB. RLS isn't an issue here — the dev superuser
// bypasses, and prod will use a service role with read access to all
// accounts (the management UI shows them to platform admins anyway).
func lookupAccountNames(ctx context.Context, db *sql.DB, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx,
		"SELECT id::text, name FROM accounts WHERE id = ANY($1::uuid[])",
		pq.StringArray(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// lookupLineItemAccount finds which account owns this line item so we can
// set the correct tenant context for the RLS-protected update. Read with no
// tenant context (the dev role bypasses RLS); production would use a
// service role with controlled cross-tenant read access.
func lookupLineItemAccount(ctx context.Context, db *sql.DB, lineItemID string) (string, error) {
	var accountID string
	// Platform-read hatch (security #77): discover-owner lookup before the caller
	// is authorised and before the write scopes itself.
	err := postgres.NewFromDB(db).QueryRowPlatform(ctx, func(row *sql.Row) error {
		return row.Scan(&accountID)
	}, "SELECT account_id::text FROM line_items WHERE id = $1", lineItemID)
	if err == sql.ErrNoRows {
		return "", errors.New("campaign not found")
	}
	if err != nil {
		return "", err
	}
	return accountID, nil
}

// lookupLineItemAccountAndStatus is the patch/delete-handler variant
// that also returns the current status, so the handler can detect a
// state transition and publish CampaignStateEvent only when the value
// actually changed (no-op patches don't generate noise on the bus).
func lookupLineItemAccountAndStatus(ctx context.Context, db *sql.DB, lineItemID string) (accountID, status string, err error) {
	// Platform-read hatch: this lookup DISCOVERS the row's owning account before
	// the caller is authorised (scope.CanMutate, below) and before updateLineItem
	// scopes its write, so under the NOBYPASSRLS app role (security #77) it must
	// use the platform hatch rather than RLS-filter to nothing. Reading the row
	// is safe — cross-tenant *mutation* is still gated by CanMutate.
	err = postgres.NewFromDB(db).QueryRowPlatform(ctx, func(row *sql.Row) error {
		return row.Scan(&accountID, &status)
	}, "SELECT account_id::text, status FROM line_items WHERE id = $1", lineItemID)
	if err == sql.ErrNoRows {
		return "", "", errors.New("campaign not found")
	}
	return accountID, status, err
}

func publishInvalidate(ctx context.Context, bus events.EventBus, log *slog.Logger, op, id string) {
	if bus == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{"source": "dsp-mgmt", "op": op, "id": id})
	if err := bus.Publish(ctx, events.SubjectCacheInvalidateCampaigns, payload); err != nil {
		log.Warn("publish invalidate failed (other pods will pick up on next poll)", "error", err)
	}
}

// publishCampaignStateChange emits adtech.campaign.state_changed when a
// pause/resume/archive actually flips the status. Skipped when oldState
// == newState (a no-op patch shouldn't generate bus noise) and when bus
// is nil (single-process tests). Reporting subscribes for ops dashboards
// + e2e tests assert on the recorded transition.
//
// Routes through events.Publisher so the schema_version default applies
// (see pkg/events/payloads.go CurrentSchemaVersion). Direct bus.Publish
// would skip the default and leave a 0 on the wire.
func publishCampaignStateChange(ctx context.Context, bus events.EventBus, log *slog.Logger, campaignID, accountID, oldState, newState, reason string) {
	if bus == nil || oldState == newState {
		return
	}
	pub := events.NewPublisher(bus, log)
	if err := pub.CampaignStateChanged(ctx, events.CampaignStateEvent{
		CampaignID: campaignID,
		AccountID:  accountID,
		OldState:   oldState,
		NewState:   newState,
		Reason:     reason,
		Timestamp:  time.Now(),
	}); err != nil {
		log.Warn("publish campaign state change failed", "campaign_id", campaignID, "old", oldState, "new", newState, "error", err)
		return
	}
	log.Info("published campaign state change", "campaign_id", campaignID, "old", oldState, "new", newState)
}

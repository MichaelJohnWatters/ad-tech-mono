package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
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
// management endpoints. Nil if Postgres isn't reachable — callers treat the
// nil case as "management endpoints unavailable" and return 503.
func openManagementDB(cfg *config.Config, log *slog.Logger) *sql.DB {
	dbURL := cfg.Get("database.url", "")
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
		log.Warn("dsp management db ping failed", "error", err)
		_ = db.Close()
		return nil
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
// dspID + dspName identify this pod's row in the dsps table. Used by POST
// to scope the new advertiser account to this DSP (accounts.dsp_id) so it
// shows up in this pod's filtered campaign list and nowhere else.
func campaignsCollectionHandler(cache *warm.Cache[models.Campaign], db *sql.DB, bus events.EventBus, dspID, dspName string, budget *BudgetTracker, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
			all := cache.All()
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

type createCampaignRequest struct {
	Name          string   `json:"name"`
	BaseBid       float64  `json:"base_bid"`
	DailyBudget   float64  `json:"daily_budget"`
	IncludeGeo    []string `json:"include_geo,omitempty"`
	IncludeDevice []string `json:"include_device,omitempty"`
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

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Get-or-create the DSP's "default management advertiser" — a single
	// bucket account per DSP for ad-hoc campaigns created via the UI.
	// Each DSP has its own (deterministic ID) and the account's dsp_id is
	// set to this DSP, so the campaign appears only in this DSP's
	// filtered cache.
	accountID, err := ensureMgmtAdvertiser(ctx, db, dspID, dspName)
	if err != nil {
		log.Error("ensure mgmt advertiser failed", "error", err, "dsp", dspName)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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

	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	json.NewEncoder(w).Encode(map[string]string{"id": lineItemID, "status": "created"})
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

	// IO (parent budget container)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO insertion_orders (id, account_id, name, budget, daily_budget, currency, start_date, end_date, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $4, 'USD', current_date, current_date + interval '90 days', 'active', now(), now())
ON CONFLICT (id) DO NOTHING`, ioID, accountID, "mgmt-"+req.Name, req.DailyBudget*30); err != nil {
		return fmt.Errorf("io insert: %w", err)
	}
	// Line item (the "campaign" in our parlance)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO line_items (id, account_id, insertion_order_id, name, status, format, bid_strategy, base_bid, bid_currency, daily_budget, pacing_mode, shading_mode, creative_rotation, timezone, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'live', 'display', 'cpm', $5, 'USD', $6, 'asap', 'moderate', 'bandit', 'UTC', now(), now())`,
		lineItemID, accountID, ioID, req.Name, req.BaseBid, req.DailyBudget); err != nil {
		return fmt.Errorf("line_item insert: %w", err)
	}
	// Targeting (geo + device)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO targeting_rules (id, line_item_id, account_id, include_geo, include_device, bid_modifiers, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, '{}', now(), now())`,
		targetingID, lineItemID, accountID, pq.StringArray(req.IncludeGeo), pq.StringArray(req.IncludeDevice)); err != nil {
		return fmt.Errorf("targeting insert: %w", err)
	}
	// Creative — auto-generated HTML banner
	html := fmt.Sprintf(`<div style="width:${WIDTH}px;height:${HEIGHT}px;background:linear-gradient(135deg,#4ECDC4,#556270);color:white;display:flex;align-items:center;justify-content:center;font-family:sans-serif;text-align:center;padding:8px;box-sizing:border-box;border-radius:4px;"><div><strong>%s</strong><br><small>via mgmt</small></div></div>`, req.Name)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO creatives (id, account_id, name, format, width, height, landing_url, html_content, review_status, created_at, updated_at)
VALUES ($1, $2, $3, 'display', 300, 250, $4, $5, 'approved', now(), now())`,
		creativeID, accountID, req.Name, "https://"+strings.ToLower(strings.ReplaceAll(req.Name, " ", "-"))+".test", html); err != nil {
		return fmt.Errorf("creative insert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO line_item_creatives (line_item_id, creative_id, weight) VALUES ($1, $2, 100)`,
		lineItemID, creativeID); err != nil {
		return fmt.Errorf("line_item_creatives insert: %w", err)
	}
	return tx.Commit()
}

type patchCampaignRequest struct {
	BaseBid     *float64 `json:"base_bid,omitempty"`
	DailyBudget *float64 `json:"daily_budget,omitempty"`
	Status      *string  `json:"status,omitempty"`
}

func handlePatch(w http.ResponseWriter, r *http.Request, db *sql.DB, bus events.EventBus, id string, log *slog.Logger) {
	var req patchCampaignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.BaseBid == nil && req.DailyBudget == nil && req.Status == nil {
		http.Error(w, "no fields to update", http.StatusBadRequest)
		return
	}
	if req.Status != nil {
		switch *req.Status {
		case "live", "paused", "archived", "ended":
		default:
			http.Error(w, "invalid status", http.StatusBadRequest)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	accountID, err := lookupLineItemAccount(ctx, db, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	if err := updateLineItem(ctx, db, accountID, id, req); err != nil {
		log.Error("patch campaign failed", "error", err, "id", id)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	publishInvalidate(ctx, bus, log, "patch", id)
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
	return tx.Commit()
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
	accountID, err := lookupLineItemAccount(ctx, db, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := updateLineItem(ctx, db, accountID, id, patchCampaignRequest{Status: &archived}); err != nil {
		log.Error("delete (archive) campaign failed", "error", err, "id", id)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	publishInvalidate(ctx, bus, log, "delete", id)
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
	err := db.QueryRowContext(ctx, "SELECT account_id::text FROM line_items WHERE id = $1", lineItemID).Scan(&accountID)
	if err == sql.ErrNoRows {
		return "", errors.New("campaign not found")
	}
	if err != nil {
		return "", err
	}
	return accountID, nil
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

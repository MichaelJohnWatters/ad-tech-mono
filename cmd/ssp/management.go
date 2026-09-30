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

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// Placement management endpoints. Mirror the DSP campaign-management pattern
// (see cmd/dsp/management.go) but for the supply side: publishers and their
// placements (ad slots). Real platform endpoints — the publisher simulator is
// the first consumer; eventually the Gateway publisher portal will use these
// same routes.
//
// TODO (production prerequisite, MUST land before public exposure):
//   - Require Authorization on POST/PATCH/DELETE; resolve caller identity to
//     publisher accounts the caller owns.
//   - Audit log entry per mutation (audit_log table exists, just needs wiring).
//
// Endpoints:
//   GET    /v1/ssp/placements            → list (served by main.go, kept for context)
//   POST   /v1/ssp/placements            → create a placement under a chosen publisher
//   PATCH  /v1/ssp/placements/{id}       → update floor_price / status / name
//   DELETE /v1/ssp/placements/{id}       → archive (status='archived')

// openManagementDB returns a writable Postgres connection for placement mgmt.
// Same pattern as cmd/dsp/management.openManagementDB — nil only when there's
// no URL or sql.Open itself fails (config errors). A failed boot-time ping
// does NOT discard the handle: database/sql pools reconnect on the next
// query, so a pod that boots before Postgres self-heals instead of latching
// "503 management db unavailable" until a restart (which is exactly what
// happened after whole-VM boots).
func openManagementDB(dbURL string, log *slog.Logger) *sql.DB {
	if dbURL == "" {
		log.Warn("database.url not set, ssp management endpoints disabled")
		return nil
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("ssp management db open failed", "error", err)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Warn("ssp management db ping failed at boot; handlers will retry on demand", "error", err)
		// Don't close — leave the handle for on-demand reconnects.
		return db
	}
	log.Info("ssp management db connected")
	return db
}

// publisherOption is what the create-placement modal renders as the publisher
// dropdown: the publishers a placement can be created under. Domain is shown
// alongside the name to disambiguate seed entries with similar names.
type publisherOption struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

// publishersListHandler returns the publishers the UI may attach a new
// placement to. Read-only; the pub sim doesn't (yet) create publishers, only
// placements under existing ones.
func publishersListHandler(db *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, "management db unavailable", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		// `publishers` is RLS-protected; the SSP runs as the NOBYPASSRLS role
		// adtech_app, so a BARE pooled query returns 0 rows even with an explicit
		// account_id filter — the tenant GUC must be set (or the platform hatch
		// used). A customer (publisher) session reads only its own publishers via
		// the tenant GUC; a platform (staff) session uses QueryPlatform to see all.
		const base = "SELECT id::text, name, domain FROM publishers WHERE status != 'archived'"
		var rows *sql.Rows
		var closeFn func()
		var err error
		if scope := middleware.CallerScope(r); scope.Resolved && !scope.Platform {
			rows, closeFn, err = postgres.QueryTenantDB(ctx, db, scope.AccountID,
				base+" AND account_id = $1::uuid ORDER BY name", scope.AccountID)
		} else {
			rows, closeFn, err = postgres.NewFromDB(db).QueryPlatform(ctx, base+" ORDER BY name")
		}
		if err != nil {
			log.Error("publishers list query failed", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer closeFn()
		out := []publisherOption{}
		for rows.Next() {
			var p publisherOption
			if err := rows.Scan(&p.ID, &p.Name, &p.Domain); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			out = append(out, p)
		}
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		json.NewEncoder(w).Encode(out)
	}
}

// placementByIDHandler dispatches PATCH/DELETE on /v1/ssp/placements/{id}.
// Mirrors cmd/dsp/management.campaignByIDHandler.
func placementByIDHandler(db *sql.DB, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, "management db unavailable", http.StatusServiceUnavailable)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/ssp/placements/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPatch:
			handlePlacementPatch(w, r, db, bus, id, log)
		case http.MethodDelete:
			handlePlacementDelete(w, r, db, bus, id, log)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// floorConfigInput is the structured device/geo/time floor-override map stored
// in placements.floor_config and resolved per-request by pkg/floors. Device
// keys are device names, geo keys are country codes; values are CPM floors
// (>= 0). Dayparts raise the floor during matching time windows (evaluated in
// Timezone, an IANA name; UTC if empty).
type floorConfigInput struct {
	Device   map[string]float64 `json:"device,omitempty"`
	Geo      map[string]float64 `json:"geo,omitempty"`
	Timezone string             `json:"timezone,omitempty"`
	Dayparts []daypartInput     `json:"dayparts,omitempty"`
}

// daypartInput is one time-window floor override. Days are weekdays with
// Sunday=0 … Saturday=6 (empty = every day). The hour window is
// [StartHour, EndHour); EndHour <= StartHour wraps past midnight.
type daypartInput struct {
	Days      []int   `json:"days,omitempty"`
	StartHour int     `json:"start_hour"`
	EndHour   int     `json:"end_hour"`
	Floor     float64 `json:"floor"`
}

// validate rejects negative floors, out-of-range days/hours, and unknown
// timezones; marshals to the JSONB the column stores.
func (f *floorConfigInput) validateAndJSON() (string, error) {
	if f == nil {
		return "{}", nil
	}
	for _, m := range []map[string]float64{f.Device, f.Geo} {
		for k, v := range m {
			if v < 0 {
				return "", fmt.Errorf("floor override %q must be >= 0", k)
			}
		}
	}
	if f.Timezone != "" {
		if _, err := time.LoadLocation(f.Timezone); err != nil {
			return "", fmt.Errorf("invalid timezone %q", f.Timezone)
		}
	}
	for i, dp := range f.Dayparts {
		if dp.Floor < 0 {
			return "", fmt.Errorf("daypart %d floor must be >= 0", i)
		}
		if dp.StartHour < 0 || dp.StartHour > 23 || dp.EndHour < 0 || dp.EndHour > 24 {
			return "", fmt.Errorf("daypart %d hours must be 0-23 (start) / 0-24 (end)", i)
		}
		for _, d := range dp.Days {
			if d < 0 || d > 6 {
				return "", fmt.Errorf("daypart %d day %d must be 0-6 (Sun-Sat)", i, d)
			}
		}
	}
	b, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// videoConfigInput is the structured per-placement video-slot config stored in
// placements.video_config and applied by the SSP (buildVideoImp) when building
// a video bid request. Pointer fields distinguish "unset" (use the default)
// from an explicit value; snake_case keys match buildVideoImp's lookups.
type videoConfigInput struct {
	Skippable   *bool    `json:"skippable,omitempty"`
	SkipAfter   *int     `json:"skip_after,omitempty"`
	MinDuration *int     `json:"min_duration,omitempty"`
	MaxDuration *int     `json:"max_duration,omitempty"`
	Plcmt       *int     `json:"plcmt,omitempty"`
	Linearity   *int     `json:"linearity,omitempty"`
	W           *int     `json:"w,omitempty"`
	H           *int     `json:"h,omitempty"`
	Mimes       []string `json:"mimes,omitempty"`
	Protocols   []int    `json:"protocols,omitempty"`
}

// validateAndJSON rejects out-of-range video settings and marshals to the JSONB
// the column stores. A nil receiver yields "{}".
func (v *videoConfigInput) validateAndJSON() (string, error) {
	if v == nil {
		return "{}", nil
	}
	pos := func(name string, p *int) error {
		if p != nil && *p < 0 {
			return fmt.Errorf("%s must be >= 0", name)
		}
		return nil
	}
	for _, e := range []error{
		pos("skip_after", v.SkipAfter), pos("min_duration", v.MinDuration),
		pos("max_duration", v.MaxDuration), pos("w", v.W), pos("h", v.H),
	} {
		if e != nil {
			return "", e
		}
	}
	if v.MinDuration != nil && v.MaxDuration != nil && *v.MinDuration > *v.MaxDuration {
		return "", fmt.Errorf("min_duration must be <= max_duration")
	}
	if v.Plcmt != nil && (*v.Plcmt < 0 || *v.Plcmt > 4) {
		return "", fmt.Errorf("plcmt must be 0-4 (OpenRTB 2.6)")
	}
	if v.Linearity != nil && (*v.Linearity < 0 || *v.Linearity > 2) {
		return "", fmt.Errorf("linearity must be 0-2")
	}
	for _, p := range v.Protocols {
		if p < 1 || p > 14 {
			return "", fmt.Errorf("protocols must be OpenRTB VAST protocol codes (1-14)")
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type createPlacementRequest struct {
	PublisherID    string            `json:"publisher_id"`
	Name           string            `json:"name"`
	Format         string            `json:"format"`
	Width          int               `json:"width"`
	Height         int               `json:"height"`
	Surfaces       int               `json:"surfaces,omitempty"` // in-game scene surfaces / retail slots
	FloorPrice     float64           `json:"floor_price"`
	PageURLPattern string            `json:"page_url_pattern,omitempty"`
	FloorConfig    *floorConfigInput `json:"floor_config,omitempty"`
	VideoConfig    *videoConfigInput `json:"video_config,omitempty"`
}

func handlePlacementCreate(w http.ResponseWriter, r *http.Request, db *sql.DB, bus events.EventBus, log *slog.Logger) {
	var req createPlacementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.PublisherID == "" {
		http.Error(w, "publisher_id required", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if req.Format == "" {
		req.Format = "display"
	}
	if req.Width <= 0 || req.Height <= 0 {
		http.Error(w, "width and height must be positive", http.StatusBadRequest)
		return
	}
	if req.FloorPrice < 0 {
		http.Error(w, "floor_price must be >= 0", http.StatusBadRequest)
		return
	}
	if _, err := req.FloorConfig.validateAndJSON(); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}
	if _, err := req.VideoConfig.validateAndJSON(); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Resolve the publisher's account_id — placements inherit the publisher's
	// tenant context (account_id), so we need it for the RLS-protected insert.
	accountID, err := lookupPublisherAccount(ctx, db, req.PublisherID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Create names a publisher (hence a target account), so gate it: an
	// account-scoped caller may only create placements under its own publisher.
	scope := middleware.CallerScope(r)
	if !scope.CanMutate(accountID) {
		log.Warn("create placement forbidden: caller not authorised for publisher's account", "actor", scope.Actor, "target_account", accountID, "publisher_id", req.PublisherID)
		http.Error(w, "forbidden: not authorised for this publisher", http.StatusForbidden)
		return
	}

	id, err := writeNewPlacement(ctx, db, accountID, req)
	if err != nil {
		log.Error("create placement failed", "error", err, "name", req.Name)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	publishPlacementInvalidate(ctx, bus, log, "create", id)
	if err := audit.Log(ctx, db, audit.Entry{
		AccountID: accountID, ActorID: scope.Actor, Action: "placement:create",
		ResourceType: "placement", ResourceID: id, Changes: req,
	}); err != nil {
		log.Warn("audit log write failed", "action", "placement:create", "id", id, "error", err)
	}

	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "created"})
}

// writeNewPlacement inserts a placements row under SET LOCAL tenant context.
// We let Postgres pick the UUID via gen_random_uuid() (the column default) —
// no deterministic external key, because UI-created placements don't need to
// be reproducible across seed runs.
func writeNewPlacement(ctx context.Context, db *sql.DB, accountID string, req createPlacementRequest) (string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_account_id', $1, true)", accountID); err != nil {
		return "", fmt.Errorf("set tenant: %w", err)
	}

	var id string
	page := req.PageURLPattern
	if page == "" {
		page = "/" // sensible default; SSP doesn't enforce a particular shape
	}
	floorJSON, _ := req.FloorConfig.validateAndJSON() // already validated in handler
	videoJSON, _ := req.VideoConfig.validateAndJSON() // already validated in handler
	err = tx.QueryRowContext(ctx, `
INSERT INTO placements (publisher_id, account_id, name, format, width, height, surfaces, floor_price, floor_currency, page_url_pattern, floor_config, video_config, status, created_at, updated_at)
VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, 'USD', $9, $10::jsonb, $11::jsonb, 'active', now(), now())
RETURNING id::text`,
		req.PublisherID, accountID, req.Name, req.Format, req.Width, req.Height, req.Surfaces, req.FloorPrice, page, floorJSON, videoJSON,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("placement insert: %w", err)
	}
	return id, tx.Commit()
}

type patchPlacementRequest struct {
	Name        *string           `json:"name,omitempty"`
	FloorPrice  *float64          `json:"floor_price,omitempty"`
	Status      *string           `json:"status,omitempty"`
	FloorConfig *floorConfigInput `json:"floor_config,omitempty"`
	VideoConfig *videoConfigInput `json:"video_config,omitempty"`
}

func handlePlacementPatch(w http.ResponseWriter, r *http.Request, db *sql.DB, bus events.EventBus, id string, log *slog.Logger) {
	var req patchPlacementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Name == nil && req.FloorPrice == nil && req.Status == nil && req.FloorConfig == nil && req.VideoConfig == nil {
		http.Error(w, "no fields to update", http.StatusBadRequest)
		return
	}
	if req.Status != nil {
		switch *req.Status {
		case "active", "paused", "archived":
		default:
			http.Error(w, "invalid status (must be active|paused|archived)", http.StatusBadRequest)
			return
		}
	}
	if req.FloorPrice != nil && *req.FloorPrice < 0 {
		http.Error(w, "floor_price must be >= 0", http.StatusBadRequest)
		return
	}
	if _, err := req.FloorConfig.validateAndJSON(); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}
	if _, err := req.VideoConfig.validateAndJSON(); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	accountID, err := lookupPlacementAccount(ctx, db, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// Tenant isolation: account-scoped callers may only mutate their own
	// publisher's placements; platform operator keys may mutate any.
	scope := middleware.CallerScope(r)
	if !scope.CanMutate(accountID) {
		log.Warn("patch placement forbidden: caller not authorised for account", "actor", scope.Actor, "target_account", accountID, "id", id)
		http.Error(w, "forbidden: not authorised for this account", http.StatusForbidden)
		return
	}

	if err := updatePlacement(ctx, db, accountID, id, req); err != nil {
		log.Error("patch placement failed", "error", err, "id", id)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	publishPlacementInvalidate(ctx, bus, log, "patch", id)
	if err := audit.Log(ctx, db, audit.Entry{
		AccountID: accountID, ActorID: scope.Actor, Action: "placement:update",
		ResourceType: "placement", ResourceID: id, Changes: req,
	}); err != nil {
		log.Warn("audit log write failed", "action", "placement:update", "id", id, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func updatePlacement(ctx context.Context, db *sql.DB, accountID, id string, req patchPlacementRequest) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_account_id', $1, true)", accountID); err != nil {
		return err
	}
	sets := []string{"updated_at = now()"}
	args := []any{}
	if req.Name != nil {
		args = append(args, *req.Name)
		sets = append(sets, fmt.Sprintf("name = $%d", len(args)))
	}
	if req.FloorPrice != nil {
		args = append(args, *req.FloorPrice)
		sets = append(sets, fmt.Sprintf("floor_price = $%d", len(args)))
	}
	if req.Status != nil {
		args = append(args, *req.Status)
		sets = append(sets, fmt.Sprintf("status = $%d", len(args)))
	}
	if req.FloorConfig != nil {
		floorJSON, _ := req.FloorConfig.validateAndJSON()
		args = append(args, floorJSON)
		sets = append(sets, fmt.Sprintf("floor_config = $%d::jsonb", len(args)))
	}
	if req.VideoConfig != nil {
		videoJSON, _ := req.VideoConfig.validateAndJSON()
		args = append(args, videoJSON)
		sets = append(sets, fmt.Sprintf("video_config = $%d::jsonb", len(args)))
	}
	args = append(args, id)
	q := fmt.Sprintf("UPDATE placements SET %s WHERE id = $%d", strings.Join(sets, ", "), len(args))
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("placement not found or RLS blocked update")
	}
	return tx.Commit()
}

func handlePlacementDelete(w http.ResponseWriter, r *http.Request, db *sql.DB, bus events.EventBus, id string, log *slog.Logger) {
	// Soft delete: status='archived'. The warm cache filters status='active'
	// at bid time so archived placements stop receiving traffic without
	// losing the row (audit, billing, FK references stay intact).
	archived := "archived"
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	accountID, err := lookupPlacementAccount(ctx, db, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	scope := middleware.CallerScope(r)
	if !scope.CanMutate(accountID) {
		log.Warn("delete placement forbidden: caller not authorised for account", "actor", scope.Actor, "target_account", accountID, "id", id)
		http.Error(w, "forbidden: not authorised for this account", http.StatusForbidden)
		return
	}
	if err := updatePlacement(ctx, db, accountID, id, patchPlacementRequest{Status: &archived}); err != nil {
		log.Error("delete (archive) placement failed", "error", err, "id", id)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	publishPlacementInvalidate(ctx, bus, log, "delete", id)
	if err := audit.Log(ctx, db, audit.Entry{
		AccountID: accountID, ActorID: scope.Actor, Action: "placement:delete",
		ResourceType: "placement", ResourceID: id, Reason: "archive",
	}); err != nil {
		log.Warn("audit log write failed", "action", "placement:delete", "id", id, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// lookupPublisherAccount returns the account_id for the publisher row. We
// need this because placements.account_id is the RLS key, and it must match
// the parent publisher's tenant (FK is publisher_id; the account is inherited
// at the application layer, not enforced by the schema).
func lookupPublisherAccount(ctx context.Context, db *sql.DB, publisherID string) (string, error) {
	var accountID string
	// Platform-read hatch: this DISCOVERS the publisher's owning account before
	// the caller is authorised (CallerScope, in the handler) and before
	// writeNewPlacement scopes its write, so under the NOBYPASSRLS app role
	// (security #77) it must use the hatch or RLS filters it to nothing.
	err := postgres.NewFromDB(db).QueryRowPlatform(ctx, func(row *sql.Row) error {
		return row.Scan(&accountID)
	}, "SELECT account_id::text FROM publishers WHERE id = $1::uuid", publisherID)
	if err == sql.ErrNoRows {
		return "", errors.New("publisher not found")
	}
	if err != nil {
		return "", err
	}
	return accountID, nil
}

// lookupPlacementAccount finds which account owns the placement, for setting
// tenant context on the RLS-protected update. Same pattern as the DSP
// lookupLineItemAccount.
func lookupPlacementAccount(ctx context.Context, db *sql.DB, id string) (string, error) {
	var accountID string
	// Platform-read hatch (security #77): discover-owner lookup before the caller
	// is authorised and before the write scopes itself.
	err := postgres.NewFromDB(db).QueryRowPlatform(ctx, func(row *sql.Row) error {
		return row.Scan(&accountID)
	}, "SELECT account_id::text FROM placements WHERE id = $1::uuid", id)
	if err == sql.ErrNoRows {
		return "", errors.New("placement not found")
	}
	if err != nil {
		return "", err
	}
	return accountID, nil
}

func publishPlacementInvalidate(ctx context.Context, bus events.EventBus, log *slog.Logger, op, id string) {
	if bus == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{"source": "ssp-mgmt", "op": op, "id": id})
	if err := bus.Publish(ctx, events.SubjectCacheInvalidatePlacements, payload); err != nil {
		log.Warn("publish placement invalidate failed (other pods will pick up on next poll)", "error", err)
	}
}

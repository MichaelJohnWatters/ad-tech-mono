package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/houseads"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// houseAdStore is the CRUD surface the gateway needs. The concrete impl is
// pkg/houseads.Store; the interface lets house_ads_test.go inject a fake
// (no Postgres for the handler unit tests).
type houseAdStore interface {
	List(ctx context.Context) ([]houseads.HouseAd, error)
	Create(ctx context.Context, in houseads.Input) (string, error)
	Update(ctx context.Context, id string, in houseads.Input) error
	Delete(ctx context.Context, id string) error
}

// validateHouseAd enforces the required fields + a known format. Markup is
// stored verbatim (HTML for display/native, inline VAST for video/audio) — we
// don't parse it here; the serving path is responsible for rendering.
func validateHouseAd(in houseads.Input) error {
	if !houseads.ValidFormat(in.Format) {
		return errBadField("format must be display, video, native or audio")
	}
	if strings.TrimSpace(in.Name) == "" {
		return errBadField("name required")
	}
	if strings.TrimSpace(in.Markup) == "" {
		return errBadField("markup required")
	}
	if in.Weight < 0 {
		return errBadField("weight must be >= 0")
	}
	return nil
}

// houseAdsHandler serves GET list (support:read) and POST create
// (support:update) at /v1/api/house-ads. House ads are the platform's own
// fallback creatives — platform-global and staff-gated, not tenant-scoped.
// Every mutation is audit-logged and publishes the house-ads cache invalidate
// so the publisher ad server reloads within NATS RTT.
func houseAdsHandler(store houseAdStore, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "support:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			ads, err := store.List(r.Context())
			if err != nil {
				log.Error("house ads list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(ads)

		case http.MethodPost:
			if !can(claims, "support:update") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var in houseads.Input
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if err := validateHouseAd(in); err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			id, err := store.Create(r.Context(), in)
			if err != nil {
				log.Error("house ad create failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			auditHouseAd(r.Context(), store, claims.UserID, "house_ad:create", id, in)
			publishHouseAdInvalidate(r.Context(), bus, id)
			log.Info("house ad created", "id", id, "format", in.Format, "actor", claims.UserID)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "created"})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// fillConfig is the minimal config surface the master on/off toggle needs.
// pgFillConfig implements it over the platform config store; a fake stands in
// for the handler unit test.
type fillConfig interface {
	// Value returns the current global value for key ("" if unset).
	Value(ctx context.Context, key string) (string, error)
	// SetGlobalClearPerPod sets the GLOBAL row for key and removes any per-pod
	// override rows, so the value actually takes effect platform-wide.
	SetGlobalClearPerPod(ctx context.Context, key, value string) error
}

// houseAdsFillHandler is the master switch for serving house ads on a no-bid.
// GET (support:read) returns the current enabled state; PUT {enabled}
// (support:update) sets the global publisher_adserver.stub_on_nobid AND clears
// per-pod overrides for that key (a per-pod row would otherwise shadow the
// global value, so a plain global write looks like it does nothing). It then
// publishes the config cache-invalidate so every pod re-polls within NATS RTT.
func houseAdsFillHandler(cfg fillConfig, db *sql.DB, bus events.EventBus, key string, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "support:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			v, err := cfg.Value(r.Context(), key)
			if err != nil {
				log.Error("house ad fill read failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": strings.EqualFold(v, "true")})

		case http.MethodPut:
			if !can(claims, "support:update") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var body struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			value := "false"
			if body.Enabled {
				value = "true"
			}
			if err := cfg.SetGlobalClearPerPod(r.Context(), key, value); err != nil {
				log.Error("house ad fill toggle failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			// Nudge every pod to re-poll immediately (config manager subscribes
			// to this subject); the 30s poll is the fallback.
			if bus != nil {
				_ = bus.Publish(r.Context(), events.SubjectCacheInvalidateConfig, []byte(key))
			}
			if db != nil {
				_ = audit.Log(r.Context(), db, audit.Entry{
					ActorID: claims.UserID, Action: "house_ad:fill_toggle",
					ResourceType: "config", ResourceID: key,
					Changes: map[string]bool{"enabled": body.Enabled},
				})
			}
			log.Info("house ad fill toggled", "enabled", body.Enabled, "actor", claims.UserID)
			_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": body.Enabled})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// pgFillConfig adapts the platform config store to fillConfig.
type pgFillConfig struct{ src *config.PostgresSource }

func (c pgFillConfig) Value(ctx context.Context, key string) (string, error) {
	all, err := c.src.FetchAll(ctx)
	if err != nil {
		return "", err
	}
	return all[key], nil
}

func (c pgFillConfig) SetGlobalClearPerPod(ctx context.Context, key, value string) error {
	// Delete removes ALL rows for the key (per-pod + global); Update then writes
	// the fresh global row. Net result: one global row, no per-pod shadows.
	if err := c.src.Delete(ctx, key); err != nil {
		return err
	}
	return c.src.Update(ctx, key, value)
}

// houseAdByIDHandler serves PUT /v1/api/house-ads/{id} (update) and
// DELETE /v1/api/house-ads/{id} (delete), both support:update. Mutations are
// audited + publish the house-ads cache invalidate.
func houseAdByIDHandler(store houseAdStore, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !can(claims, "support:update") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, routes.APIHouseAds+"/")
		if id == "" || strings.Contains(id, "/") || !uuidRe.MatchString(id) {
			http.Error(w, `{"error":"id must be a house-ad UUID"}`, http.StatusBadRequest)
			return
		}

		switch r.Method {
		case http.MethodPut:
			var in houseads.Input
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			if err := validateHouseAd(in); err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			err := store.Update(r.Context(), id, in)
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, `{"error":"house ad not found"}`, http.StatusNotFound)
				return
			}
			if err != nil {
				log.Error("house ad update failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			auditHouseAd(r.Context(), store, claims.UserID, "house_ad:update", id, in)
			publishHouseAdInvalidate(r.Context(), bus, id)
			log.Info("house ad updated", "id", id, "format", in.Format, "actor", claims.UserID)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "updated"})

		case http.MethodDelete:
			err := store.Delete(r.Context(), id)
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, `{"error":"house ad not found"}`, http.StatusNotFound)
				return
			}
			if err != nil {
				log.Error("house ad delete failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			auditHouseAd(r.Context(), store, claims.UserID, "house_ad:delete", id, nil)
			publishHouseAdInvalidate(r.Context(), bus, id)
			log.Info("house ad deleted", "id", id, "actor", claims.UserID)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "deleted"})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// publishHouseAdInvalidate broadcasts the house-ads cache invalidate so the
// publisher ad server's warm cache reloads. Best-effort; a nil bus (NATS
// unreachable at boot) is tolerated — the ad server's poll tick still catches
// the change.
func publishHouseAdInvalidate(ctx context.Context, bus events.EventBus, id string) {
	if bus == nil {
		return
	}
	_ = bus.Publish(ctx, events.SubjectCacheInvalidateHouseAds, []byte(`{"source":"gateway","id":"`+id+`"}`))
}

// auditHouseAd writes the mutation to the audit log. Best-effort — the mutation
// already committed, so a failed audit write is logged, not fatal. The store's
// DB handle backs the audit write when available (pgHouseAdStore exposes it);
// the fake store used in unit tests returns nil, skipping the audit call.
func auditHouseAd(ctx context.Context, store houseAdStore, actor, action, id string, changes any) {
	db, ok := store.(interface{ DB() *sql.DB })
	if !ok || db.DB() == nil {
		return
	}
	_ = audit.Log(ctx, db.DB(), audit.Entry{
		ActorID:      actor,
		Action:       action,
		ResourceType: "house_ad",
		ResourceID:   id,
		Changes:      changes,
	})
}

// pgHouseAdStore adapts pkg/houseads.Store to the handler's houseAdStore
// interface and exposes the underlying DB for the audit write.
type pgHouseAdStore struct {
	store *houseads.Store
	db    *sql.DB
}

func newPGHouseAdStore(db *sql.DB) pgHouseAdStore {
	return pgHouseAdStore{store: houseads.NewStore(db), db: db}
}

func (s pgHouseAdStore) List(ctx context.Context) ([]houseads.HouseAd, error) {
	return s.store.List(ctx)
}
func (s pgHouseAdStore) Create(ctx context.Context, in houseads.Input) (string, error) {
	return s.store.Create(ctx, in)
}
func (s pgHouseAdStore) Update(ctx context.Context, id string, in houseads.Input) error {
	return s.store.Update(ctx, id, in)
}
func (s pgHouseAdStore) Delete(ctx context.Context, id string) error {
	return s.store.Delete(ctx, id)
}
func (s pgHouseAdStore) DB() *sql.DB { return s.db }

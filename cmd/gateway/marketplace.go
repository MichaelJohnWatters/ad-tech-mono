package main

// marketplace.go — the data marketplace API (PLAN Phase 10, slice 1).
//
//	GET  /v1/api/marketplace/listings          — browse the catalog (cross-tenant active listings)
//	GET  /v1/api/marketplace/listings?scope=mine — the caller's own listings (seller view)
//	POST /v1/api/marketplace/listings          — list one of the caller's PUBLIC segments
//
// A listing publishes a data owner's PUBLIC audience segment for other tenants
// to discover + (later slices) buy targeting access to. Only aggregate
// characteristics ride the listing — never individual members.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/marketplace"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// marketplaceListingRequest is a POST body: list (or re-list) a public segment.
type marketplaceListingRequest struct {
	SegmentID    string          `json:"segment_id"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	CPMSurcharge float64         `json:"cpm_surcharge"` // dollars; converted to micros
	Preview      json.RawMessage `json:"preview,omitempty"`
	Status       string          `json:"status,omitempty"` // active|paused|withdrawn (default active)
}

// marketplaceHandler serves the tenant-scoped marketplace listings API. It uses
// the marketplace store for listings and the audience store to validate that a
// listed segment is the caller's own PUBLIC segment.
func marketplaceHandler(store marketplace.Store, audStore *audiencepg.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, []marketplace.Listing{}) {
			return
		}
		if store == nil || audStore == nil {
			http.Error(w, `{"error":"marketplace unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "marketplace:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			if r.URL.Query().Get("scope") == "mine" {
				listings, err := store.ListByAccount(r.Context(), claims.AccountID, 200)
				if err != nil {
					log.Error("marketplace list mine failed", "error", err)
					http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
					return
				}
				_ = json.NewEncoder(w).Encode(listings)
				return
			}
			// Catalog: cross-tenant active listings, excluding the caller's own.
			catalog, err := store.Catalog(r.Context(), claims.AccountID, 200)
			if err != nil {
				log.Error("marketplace catalog failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(catalog)

		case http.MethodPost:
			if !can(claims, "marketplace:list") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			handleMarketplaceListing(w, r, store, audStore, claims.AccountID, log)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// handleMarketplaceListing validates the segment (caller's own + public) and
// upserts the listing, snapshotting the segment's live member count as the
// advertised reach.
func handleMarketplaceListing(w http.ResponseWriter, r *http.Request, store marketplace.Store, audStore *audiencepg.Store, accountID string, log *slog.Logger) {
	var req marketplaceListingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	req.SegmentID = strings.TrimSpace(req.SegmentID)
	req.Name = strings.TrimSpace(req.Name)
	if req.SegmentID == "" || req.Name == "" {
		http.Error(w, `{"error":"segment_id and name are required"}`, http.StatusBadRequest)
		return
	}
	if req.CPMSurcharge < 0 {
		http.Error(w, `{"error":"cpm_surcharge must be >= 0"}`, http.StatusBadRequest)
		return
	}
	if req.Status == "" {
		req.Status = marketplace.StatusActive
	}
	if !marketplace.IsValidStatus(req.Status) {
		http.Error(w, `{"error":"status must be active, paused or withdrawn"}`, http.StatusBadRequest)
		return
	}

	// The listed segment must be the caller's OWN and PUBLIC — you can only sell
	// data you own, and a dsp_private segment can't be shared cross-tenant.
	segs, err := audStore.ListSegments(r.Context(), accountID)
	if err != nil {
		log.Error("marketplace: segment validation failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	var seg *audiencepg.Segment
	for i := range segs {
		if segs[i].ID == req.SegmentID {
			seg = &segs[i]
			break
		}
	}
	if seg == nil {
		http.Error(w, `{"error":"segment not found (must be one of your own segments)"}`, http.StatusNotFound)
		return
	}
	if seg.Visibility != "public" {
		http.Error(w, `{"error":"only PUBLIC segments can be listed — set the segment's visibility to public first"}`, http.StatusBadRequest)
		return
	}

	id, err := store.UpsertListing(r.Context(), marketplace.Listing{
		AccountID:          accountID,
		SegmentID:          req.SegmentID,
		Name:               req.Name,
		Description:        req.Description,
		SizeEstimate:       int64(seg.Members), // live member count = honest reach
		CPMSurchargeMicros: int64(req.CPMSurcharge*1e6 + 0.5),
		Preview:            req.Preview,
		Status:             req.Status,
	})
	if err != nil {
		log.Error("marketplace: upsert listing failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	log.Info("marketplace listing upserted", "id", id, "segment", req.SegmentID, "account", accountID)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
}

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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/marketplace"
	marketplacepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/marketplace/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
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

// estimateMinAggregation is the privacy floor for an expansion estimate: an
// overlap smaller than this is SUPPRESSED (the buyer learns it's below the
// threshold, never the exact small number). Mirrors the clean-room "results
// suppressed if any group < 100 users" guarantee (PLAN Phase 10). A const for
// now; promote to live config if a tenant needs a different floor.
const estimateMinAggregation = 100

// marketplaceEstimateResponse is the expansion-estimate result — aggregate-only,
// never a member list.
type marketplaceEstimateResponse struct {
	ListingName       string   `json:"listing_name"`
	YourAudienceSize  int      `json:"your_audience_size"`
	ListingSize       int      `json:"listing_size"`
	Overlap           *int     `json:"overlap,omitempty"`     // nil when suppressed
	OverlapPct        *float64 `json:"overlap_pct,omitempty"` // nil when suppressed
	OverlapSuppressed bool     `json:"overlap_suppressed"`
	NewReachableUsers int      `json:"new_reachable_users"`
	ExpansionFactor   float64  `json:"expansion_factor"`
	MinAggregation    int      `json:"min_aggregation"`
}

// marketplaceListingActionHandler dispatches per-listing subtree actions:
// POST /v1/api/marketplace/listings/{id}/purchase (slice 2) and
// POST /v1/api/marketplace/listings/{id}/estimate  (slice 4).
func marketplaceListingActionHandler(store *marketplacepg.Store, audStore *audiencepg.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, map[string]string{}) {
			return
		}
		if store == nil {
			http.Error(w, `{"error":"marketplace unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		// Path: /v1/api/marketplace/listings/{id}/{action}
		rest := strings.TrimPrefix(r.URL.Path, routes.APIMarketplaceListingsSub)
		parts := strings.Split(strings.Trim(rest, "/"), "/")
		if len(parts) != 2 || parts[0] == "" {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		listingID, action := parts[0], parts[1]

		listing, err := store.GetByID(r.Context(), listingID)
		if err != nil {
			log.Error("marketplace action: load listing failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if listing == nil {
			http.Error(w, `{"error":"listing not found or not active"}`, http.StatusNotFound)
			return
		}

		switch action {
		case "purchase":
			marketplaceDoPurchase(w, r, store, listing, claims, log)
		case "estimate":
			marketplaceDoEstimate(w, r, audStore, listing, claims, log)
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}
}

// marketplaceDoPurchase (slice 2): the caller buys targeting access → a grant.
func marketplaceDoPurchase(w http.ResponseWriter, r *http.Request, store *marketplacepg.Store, listing *marketplace.Listing, claims *auth.Claims, log *slog.Logger) {
	if !can(claims, "marketplace:buy") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	if listing.AccountID == claims.AccountID {
		http.Error(w, `{"error":"you cannot purchase your own listing"}`, http.StatusBadRequest)
		return
	}
	id, err := store.Purchase(r.Context(), marketplace.Grant{
		ListingID:          listing.ID,
		SegmentID:          listing.SegmentID,
		SellerAccountID:    listing.AccountID,
		BuyerAccountID:     claims.AccountID,
		CPMSurchargeMicros: listing.CPMSurchargeMicros,
	})
	if err != nil {
		log.Error("marketplace purchase failed", "listing", listing.ID, "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	log.Info("marketplace purchase", "grant", id, "listing", listing.ID, "buyer", claims.AccountID, "seller", listing.AccountID)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"grant_id": id, "segment_id": listing.SegmentID})
}

// marketplaceDoEstimate (slice 4): a clean-room-lite overlap/expansion estimate
// between the caller's own audience and the listing's segment — aggregate-only,
// with a min-aggregation privacy floor. No purchase required.
//
// This computes the overlap in-process from data the platform already holds
// (both segments' member sets); it is NOT the full clean-room guarantee. The
// production clean room runs the match in an ISOLATED job with no network access
// so no raw data can leave, returning only aggregates — a deliberately deferred
// hardening (documented boundary), not built here. The min-aggregation floor +
// aggregate-only response are the privacy guarantees that ARE enforced.
func marketplaceDoEstimate(w http.ResponseWriter, r *http.Request, audStore *audiencepg.Store, listing *marketplace.Listing, claims *auth.Claims, log *slog.Logger) {
	if !can(claims, "marketplace:read") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	if audStore == nil {
		http.Error(w, `{"error":"audience store unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	var req struct {
		MyAudienceID string `json:"my_audience_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	req.MyAudienceID = strings.TrimSpace(req.MyAudienceID)
	if req.MyAudienceID == "" {
		http.Error(w, `{"error":"my_audience_id is required (one of your own segments)"}`, http.StatusBadRequest)
		return
	}
	// The buyer can only estimate against their OWN audience (no probing another
	// tenant's data). Validate ownership.
	segs, err := audStore.ListSegments(r.Context(), claims.AccountID)
	if err != nil {
		log.Error("marketplace estimate: segment validation failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	owned := false
	for _, s := range segs {
		if s.ID == req.MyAudienceID {
			owned = true
			break
		}
	}
	if !owned {
		http.Error(w, `{"error":"my_audience_id must be one of your own segments"}`, http.StatusNotFound)
		return
	}

	o, err := audStore.EstimateOverlap(r.Context(), req.MyAudienceID, listing.SegmentID)
	if err != nil {
		log.Error("marketplace estimate: overlap failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	resp := marketplaceEstimateResponse{
		ListingName:      listing.Name,
		YourAudienceSize: o.SizeA,
		ListingSize:      o.SizeB,
		MinAggregation:   estimateMinAggregation,
	}
	// Privacy floor: suppress an overlap below the threshold — the buyer learns
	// it's small, not the exact number. new_reachable/expansion then use a
	// conservative overlap=0 so the suppressed count can't be back-derived.
	effectiveOverlap := o.Overlap
	if o.Overlap < estimateMinAggregation {
		resp.OverlapSuppressed = true
		effectiveOverlap = 0
	} else {
		ov := o.Overlap
		resp.Overlap = &ov
		if o.SizeA > 0 {
			pct := float64(o.Overlap) / float64(o.SizeA) * 100
			resp.OverlapPct = &pct
		}
	}
	resp.NewReachableUsers = o.SizeB - effectiveOverlap
	if resp.NewReachableUsers < 0 {
		resp.NewReachableUsers = 0
	}
	if o.SizeA > 0 {
		resp.ExpansionFactor = float64(o.SizeA+resp.NewReachableUsers) / float64(o.SizeA)
	}
	log.Info("marketplace estimate", "listing", listing.ID, "buyer", claims.AccountID,
		"your_size", o.SizeA, "overlap_suppressed", resp.OverlapSuppressed, "new_reachable", resp.NewReachableUsers)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// marketplaceGrantsHandler serves GET /v1/api/marketplace/grants — the caller's
// purchased data (buyer view, default) or ?scope=sales (seller view).
func marketplaceGrantsHandler(store *marketplacepg.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, []marketplace.Grant{}) {
			return
		}
		if store == nil {
			http.Error(w, `{"error":"marketplace unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !can(claims, "marketplace:read") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		var grants []marketplace.Grant
		var err error
		if r.URL.Query().Get("scope") == "sales" {
			grants, err = store.SellerSales(r.Context(), claims.AccountID, 200)
		} else {
			grants, err = store.BuyerGrants(r.Context(), claims.AccountID, 200)
		}
		if err != nil {
			log.Error("marketplace grants failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(grants)
	}
}

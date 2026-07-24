package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// taxonomyHandler serves GET /v1/api/taxonomy — the IAB Audience Taxonomy
// reference nodes (migration 062) the portal picker fuzzy-filters over.
// Global reference data: identical for every account, so no tenant scoping
// beyond requiring an authenticated portal user.
func taxonomyHandler(store *audiencepg.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if store == nil {
			http.Error(w, `{"error":"audience store unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		nodes, err := store.ListTaxonomy(r.Context())
		if err != nil {
			log.Error("taxonomy list failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(nodes)
	}
}

// audienceTaxonomyRequest sets (taxonomy_id present) or clears (taxonomy_id
// null/absent) a segment's IAB Audience Taxonomy label.
type audienceTaxonomyRequest struct {
	SegmentID  string `json:"segment_id"`
	TaxonomyID *int64 `json:"taxonomy_id"`
}

// audienceTaxonomyHandler serves PUT /v1/api/audiences/taxonomy. The segment
// is resolved under the authenticated account — labelling another tenant's
// segment is a 404, same contract as every other segment write. A successful
// write publishes the audience cache invalidate so every SSP pod's taxonomy
// warm map refreshes in seconds (multi-pod: the 30s tick alone would leave
// replicas stamping stale user.data). bus is nil-tolerant (poll remains the
// staleness ceiling).
func audienceTaxonomyHandler(store *audiencepg.Store, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPut {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !can(claims, "audiences:upload") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if store == nil {
			http.Error(w, `{"error":"audience store unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		var req audienceTaxonomyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SegmentID == "" {
			http.Error(w, `{"error":"segment_id required"}`, http.StatusBadRequest)
			return
		}
		if err := store.SetSegmentTaxonomy(r.Context(), claims.AccountID, req.SegmentID, req.TaxonomyID); err != nil {
			if strings.Contains(err.Error(), "not found") {
				http.Error(w, `{"error":"segment not found"}`, http.StatusNotFound)
				return
			}
			log.Error("set segment taxonomy failed", "segment", req.SegmentID, "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if bus != nil {
			payload := []byte(`{"segment_id":"` + req.SegmentID + `","account_id":"` + claims.AccountID + `"}`)
			if err := bus.Publish(r.Context(), events.SubjectCacheInvalidateAudience, payload); err != nil {
				log.Warn("taxonomy invalidate publish failed", "segment", req.SegmentID, "error", err)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

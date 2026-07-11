package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// audienceUploadRequest is a CRM/audience upload: create-or-find a named
// segment for an account and bulk-add user memberships. This is the
// production write path the audience pipeline (CRM import, behavioural
// rollup, lookalike publish) uses — previously memberships only ever
// arrived via seed/e2e raw SQL.
//
// account_id is IGNORED — the handler binds the segment to the authenticated
// account from the JWT claims, so a caller can never write another tenant's
// data. The field remains for backward-compatible request bodies.
type audienceUploadRequest struct {
	AccountID  string   `json:"account_id"`
	Name       string   `json:"name"`
	Type       string   `json:"type,omitempty"`       // default first_party
	Visibility string   `json:"visibility,omitempty"` // public | dsp_private (default dsp_private)
	Source     string   `json:"source,omitempty"`     // default crm_upload
	UserIDs    []string `json:"user_ids"`
}

type audienceUploadResponse struct {
	SegmentID    string `json:"segment_id"`
	MembersAdded int    `json:"members_added"`
	MembersSent  int    `json:"members_sent"`
}

// audienceHandler serves the tenant-scoped audiences API for the customer
// portal:
//
//	GET  /v1/api/audiences — list the account's segments (with member counts)
//	POST /v1/api/audiences — upload a named segment + bulk-add members
//
// Both bind to the authenticated account from the JWT claims; a body
// account_id is ignored, so a caller can never read or write another tenant's
// data. A member upsert publishes an audience cache-invalidate so the DSP/SSP
// warm caches pick the new members up within a round-trip.
func audienceHandler(store *audiencepg.Store, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, []audiencepg.Segment{}) {
			return
		}
		if store == nil {
			http.Error(w, `{"error":"audience store unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "audiences:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			segs, err := store.ListSegments(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("audience list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(segs)

		case http.MethodPost:
			if !can(claims, "audiences:upload") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var req audienceUploadRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
				return
			}
			// Bind to the authenticated tenant — never trust a body account_id.
			req.AccountID = claims.AccountID
			if req.Name == "" {
				http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
				return
			}
			if len(req.UserIDs) == 0 {
				http.Error(w, `{"error":"user_ids must be non-empty"}`, http.StatusBadRequest)
				return
			}
			if req.Type == "" {
				req.Type = "first_party"
			}
			if !isValidSegmentType(req.Type) {
				http.Error(w, `{"error":"invalid type"}`, http.StatusBadRequest)
				return
			}
			if req.Visibility == "" {
				req.Visibility = "dsp_private"
			}
			if req.Visibility != "public" && req.Visibility != "dsp_private" {
				http.Error(w, `{"error":"visibility must be public or dsp_private"}`, http.StatusBadRequest)
				return
			}
			if req.Source == "" {
				req.Source = "crm_upload"
			}

			segmentID, err := store.UpsertSegment(r.Context(), req.AccountID, req.Name, req.Type, req.Source, req.Visibility)
			if err != nil {
				log.Error("audience upload: upsert segment failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			added, err := store.AddMembers(r.Context(), req.AccountID, segmentID, req.UserIDs)
			if err != nil {
				log.Error("audience upload: add members failed", "segment", segmentID, "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}

			if bus != nil {
				payload := []byte(`{"segment_id":"` + segmentID + `","account_id":"` + req.AccountID + `"}`)
				if err := bus.Publish(r.Context(), events.SubjectCacheInvalidateAudience, payload); err != nil {
					log.Warn("audience upload: invalidate publish failed", "segment", segmentID, "error", err)
				}
			}

			log.Info("audience upload", "segment", segmentID, "name", req.Name, "added", added, "sent", len(req.UserIDs))
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(audienceUploadResponse{
				SegmentID:    segmentID,
				MembersAdded: added,
				MembersSent:  len(req.UserIDs),
			})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

func isValidSegmentType(t string) bool {
	switch t {
	case "first_party", "behavioral", "lookalike", "suppression",
		"retargeting", "composite", "predictive", "cdp_imported":
		return true
	}
	return false
}

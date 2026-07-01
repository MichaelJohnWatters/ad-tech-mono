package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

// audienceUploadRequest is a CRM/audience upload: create-or-find a named
// segment for an account and bulk-add user memberships. This is the
// production write path the audience pipeline (CRM import, behavioural
// rollup, lookalike publish) uses — previously memberships only ever
// arrived via seed/e2e raw SQL.
//
// account_id is taken from the body for now (an operator uploading on an
// advertiser's behalf); a future dashboard-facing version should bind it to
// the authenticated account from the JWT instead of trusting the caller.
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

// audienceHandler handles POST /v1/api/audiences. Creates/updates the
// segment, bulk-inserts members, and publishes an audience cache-invalidate
// so DSP/SSP warm caches pick the new members up within a round-trip.
func audienceHandler(store *audiencepg.Store, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if store == nil {
			http.Error(w, "audience store unavailable", http.StatusServiceUnavailable)
			return
		}
		var req audienceUploadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.AccountID == "" || req.Name == "" {
			http.Error(w, "account_id and name are required", http.StatusBadRequest)
			return
		}
		if len(req.UserIDs) == 0 {
			http.Error(w, "user_ids must be non-empty", http.StatusBadRequest)
			return
		}
		if req.Type == "" {
			req.Type = "first_party"
		}
		if !isValidSegmentType(req.Type) {
			http.Error(w, "invalid type", http.StatusBadRequest)
			return
		}
		if req.Visibility == "" {
			req.Visibility = "dsp_private"
		}
		if req.Visibility != "public" && req.Visibility != "dsp_private" {
			http.Error(w, "visibility must be public or dsp_private", http.StatusBadRequest)
			return
		}
		if req.Source == "" {
			req.Source = "crm_upload"
		}

		segmentID, err := store.UpsertSegment(r.Context(), req.AccountID, req.Name, req.Type, req.Source, req.Visibility)
		if err != nil {
			log.Error("audience upload: upsert segment failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		added, err := store.AddMembers(r.Context(), req.AccountID, segmentID, req.UserIDs)
		if err != nil {
			log.Error("audience upload: add members failed", "segment", segmentID, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if bus != nil {
			payload := []byte(`{"segment_id":"` + segmentID + `","account_id":"` + req.AccountID + `"}`)
			if err := bus.Publish(r.Context(), events.SubjectCacheInvalidateAudience, payload); err != nil {
				log.Warn("audience upload: invalidate publish failed", "segment", segmentID, "error", err)
			}
		}

		log.Info("audience upload", "segment", segmentID, "name", req.Name, "added", added, "sent", len(req.UserIDs))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(audienceUploadResponse{
			SegmentID:    segmentID,
			MembersAdded: added,
			MembersSent:  len(req.UserIDs),
		})
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

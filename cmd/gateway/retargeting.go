package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// retargetingAudienceHandler serves the tenant-scoped retargeting-audience API
// for the advertiser portal:
//
//	GET  /v1/api/audiences/retargeting — list the account's retargeting audiences
//	     (name, pixel tag, TTL window, live enrollment count)
//	POST /v1/api/audiences/retargeting — create one {name, tag, window_days}
//
// The created segment is the target of the /v1/t/rt pixel; cmd/audience-rt enrolls
// a visitor into it in real time on each site_visit and suppresses on purchase.
// Bound to the JWT account — a body account_id is ignored.
func retargetingAudienceHandler(store *audiencepg.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, []audiencepg.RetargetingSegmentUI{}) {
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
			segs, err := store.ListRetargetingSegments(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("retargeting audience list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(segs)

		case http.MethodPost:
			if !can(claims, "audiences:create") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var req struct {
				Name       string `json:"name"`
				Tag        string `json:"tag"`
				WindowDays int    `json:"window_days"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
				http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
				return
			}
			req.Name = strings.TrimSpace(req.Name)
			req.Tag = strings.TrimSpace(req.Tag)
			if req.Name == "" || req.Tag == "" {
				http.Error(w, `{"error":"name and tag are required"}`, http.StatusBadRequest)
				return
			}
			id, err := store.CreateRetargetingSegment(r.Context(), claims.AccountID, req.Name, req.Tag, req.WindowDays)
			if err != nil {
				log.Error("retargeting audience create failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

package main

// incidents.go — staff incident management behind the public status page
// (PLAN Phase 11, item 107).
//
//	GET  /v1/api/incidents   — list incidents (incidents:read)
//	POST /v1/api/incidents   — create (no id) or update (id present) (incidents:write)
//
// Incidents are platform-global (not tenant-scoped); staff author them and they
// surface on the public /status page.

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/statuspage"
)

var validImpact = map[string]bool{"none": true, "minor": true, "major": true, "critical": true}
var validIncidentStatus = map[string]bool{"investigating": true, "identified": true, "monitoring": true, "resolved": true}

type incidentRequest struct {
	ID                 string   `json:"id,omitempty"`
	Title              string   `json:"title"`
	Body               string   `json:"body,omitempty"`
	Impact             string   `json:"impact"`
	Status             string   `json:"status"`
	AffectedComponents []string `json:"affected_components,omitempty"`
}

func incidentsHandler(store statuspage.Store, auditDB *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if store == nil {
			http.Error(w, `{"error":"incidents unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "incidents:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			incs, err := store.ListAll(r.Context(), 100)
			if err != nil {
				log.Error("incidents list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(incs)

		case http.MethodPost:
			if !can(claims, "incidents:write") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			handleIncidentWrite(w, r, store, auditDB, claims.UserID, log)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

func handleIncidentWrite(w http.ResponseWriter, r *http.Request, store statuspage.Store, auditDB *sql.DB, userID string, log *slog.Logger) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSupportBodyBytes)
	var req incidentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		http.Error(w, `{"error":"title is required"}`, http.StatusBadRequest)
		return
	}
	if len(req.Title) > maxSubjectLen {
		http.Error(w, `{"error":"title too long"}`, http.StatusBadRequest)
		return
	}
	if len(req.Body) > maxMessageLen {
		http.Error(w, `{"error":"body too long"}`, http.StatusBadRequest)
		return
	}
	if req.Impact == "" {
		req.Impact = "minor"
	}
	if req.Status == "" {
		req.Status = "investigating"
	}
	if !validImpact[req.Impact] {
		http.Error(w, `{"error":"impact must be none, minor, major or critical"}`, http.StatusBadRequest)
		return
	}
	if !validIncidentStatus[req.Status] {
		http.Error(w, `{"error":"status must be investigating, identified, monitoring or resolved"}`, http.StatusBadRequest)
		return
	}
	if req.AffectedComponents == nil {
		req.AffectedComponents = []string{}
	}

	inc := statuspage.Incident{
		ID:                 req.ID,
		Title:              req.Title,
		Body:               req.Body,
		Impact:             req.Impact,
		Status:             req.Status,
		AffectedComponents: req.AffectedComponents,
		CreatedBy:          userID,
	}

	if req.ID == "" {
		id, err := store.Create(r.Context(), inc)
		if err != nil {
			log.Error("incident create failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		auditIncident(r, auditDB, userID, "incident:create", id, req, log)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
		return
	}

	updated, err := store.Update(r.Context(), inc)
	if err != nil {
		log.Error("incident update failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if updated == nil {
		http.Error(w, `{"error":"incident not found"}`, http.StatusNotFound)
		return
	}
	auditIncident(r, auditDB, userID, "incident:update", req.ID, req, log)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(updated)
}

func auditIncident(r *http.Request, auditDB *sql.DB, userID, action, id string, req incidentRequest, log *slog.Logger) {
	if auditDB == nil {
		return
	}
	_ = audit.Log(r.Context(), auditDB, audit.Entry{
		ActorID:      "user:" + userID,
		Action:       action,
		ResourceType: "incident",
		ResourceID:   id,
		Changes:      map[string]any{"title": req.Title, "impact": req.Impact, "status": req.Status},
	})
}

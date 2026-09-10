package main

// changelog.go — the public API changelog (PLAN Phase 11, item 109).
//
//	GET  /changelog                  — public HTML page (no auth)
//	GET  /v1/api/changelog           — public JSON feed (no auth), newest-first
//	GET  /v1/api/changelog/entries   — staff list (changelog:read)
//	POST /v1/api/changelog/entries   — staff create (no id) / update (id present) (changelog:write)
//	DELETE /v1/api/changelog/entries?id=… — staff delete (changelog:write)
//
// Entries are platform-global (not tenant-scoped); staff author them and they
// surface publicly for external integrators. Public reads are intentionally
// unauthenticated — a changelog is meant to be world-readable, like /status.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/changelog"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

type changelogServer struct {
	store     changelog.Store
	templates *templateManager
	log       *slog.Logger
}

// jsonHandler serves GET /v1/api/changelog (public JSON feed, newest-first).
func (c *changelogServer) jsonHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*") // public feed, cross-origin readable
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	entries := c.recent(w, r)
	if entries == nil {
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries})
}

// pageHandler serves GET /changelog (public HTML).
func (c *changelogServer) pageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	entries := c.recent(w, r)
	if entries == nil {
		return
	}
	c.templates.Render(w, "changelog.html", map[string]any{
		"Entries":     entries,
		"GeneratedAt": time.Now().UTC(),
	})
}

// recent loads the public feed, writing an error response + returning nil on failure.
func (c *changelogServer) recent(w http.ResponseWriter, r *http.Request) []changelog.Entry {
	if c.store == nil {
		http.Error(w, `{"error":"changelog unavailable"}`, http.StatusServiceUnavailable)
		return nil
	}
	entries, err := c.store.Recent(r.Context(), 100)
	if err != nil {
		c.log.Error("changelog list failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return nil
	}
	return entries
}

// entriesHandler is the staff CRUD surface (behind authMiddleware + RBAC).
func (c *changelogServer) entriesHandler(auditDB *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if c.store == nil {
			http.Error(w, `{"error":"changelog unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		switch r.Method {
		case http.MethodGet:
			if !can(claims, "changelog:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			entries, err := c.store.Recent(r.Context(), 200)
			if err != nil {
				c.log.Error("changelog list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(entries)

		case http.MethodPost:
			if !can(claims, "changelog:write") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			c.handleWrite(w, r, auditDB, claims.UserID)

		case http.MethodDelete:
			if !can(claims, "changelog:write") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			id := r.URL.Query().Get("id")
			if id == "" {
				http.Error(w, `{"error":"id required"}`, http.StatusBadRequest)
				return
			}
			if err := c.store.Delete(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			} else if err != nil {
				c.log.Error("changelog delete failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			if auditDB != nil {
				_ = audit.Log(r.Context(), auditDB, audit.Entry{ActorID: "user:" + claims.UserID, Action: "changelog:delete", ResourceType: "changelog_entry", ResourceID: id})
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

type changelogRequest struct {
	ID                string   `json:"id,omitempty"`
	Version           string   `json:"version"`
	ReleaseDate       string   `json:"release_date"` // YYYY-MM-DD
	Category          string   `json:"category"`
	Breaking          bool     `json:"breaking"`
	Title             string   `json:"title"`
	Body              string   `json:"body,omitempty"`
	AffectedEndpoints []string `json:"affected_endpoints,omitempty"`
}

func (c *changelogServer) handleWrite(w http.ResponseWriter, r *http.Request, auditDB *sql.DB, userID string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSupportBodyBytes)
	var req changelogRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	req.Version = strings.TrimSpace(req.Version)
	req.Title = strings.TrimSpace(req.Title)
	if req.Version == "" || req.Title == "" {
		http.Error(w, `{"error":"version and title are required"}`, http.StatusBadRequest)
		return
	}
	if len(req.Version) > 40 || len(req.Title) > maxSubjectLen || len(req.Body) > maxMessageLen {
		http.Error(w, `{"error":"field too long"}`, http.StatusBadRequest)
		return
	}
	if req.Category == "" {
		req.Category = "changed"
	}
	if !changelog.ValidCategory(req.Category) {
		http.Error(w, `{"error":"category must be added, changed, deprecated, removed, fixed or security"}`, http.StatusBadRequest)
		return
	}
	date, err := time.Parse("2006-01-02", req.ReleaseDate)
	if err != nil {
		http.Error(w, `{"error":"release_date must be YYYY-MM-DD"}`, http.StatusBadRequest)
		return
	}
	if req.AffectedEndpoints == nil {
		req.AffectedEndpoints = []string{}
	}
	in := changelog.Input{
		Version: req.Version, ReleaseDate: date, Category: req.Category, Breaking: req.Breaking,
		Title: req.Title, Body: req.Body, AffectedEndpoints: req.AffectedEndpoints,
	}

	if req.ID == "" {
		id, err := c.store.Create(r.Context(), in, userID)
		if err != nil {
			c.log.Error("changelog create failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		c.audit(r, auditDB, userID, "changelog:create", id, req)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
		return
	}
	if err := c.store.Update(r.Context(), req.ID, in); errors.Is(err, sql.ErrNoRows) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	} else if err != nil {
		c.log.Error("changelog update failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	c.audit(r, auditDB, userID, "changelog:update", req.ID, req)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": req.ID})
}

func (c *changelogServer) audit(r *http.Request, auditDB *sql.DB, userID, action, id string, req changelogRequest) {
	if auditDB == nil {
		return
	}
	_ = audit.Log(r.Context(), auditDB, audit.Entry{
		ActorID:      "user:" + userID,
		Action:       action,
		ResourceType: "changelog_entry",
		ResourceID:   id,
		Changes:      map[string]any{"version": req.Version, "category": req.Category, "breaking": req.Breaking, "title": req.Title},
	})
}

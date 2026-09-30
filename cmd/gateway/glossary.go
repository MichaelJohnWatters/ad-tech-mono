package main

// glossary.go — the staff-only ad-tech glossary reference.
//
//	GET /v1/api/glossary — staff read (support:read); returns the static,
//	code-grounded term/definition set grouped by category.
//
// The content is static Go data (pkg/glossary) — no DB, no migration. It's a
// read-only learning / interview study aid, platform-global (the same list for
// every staff caller), so it's permission-gated rather than tenant-scoped. It
// mirrors the other read-only staff surfaces (audit log, batch runs): behind
// authMiddleware + a can(claims, …) check, JSON out.

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/glossary"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// glossaryHandler serves the staff glossary (GET, support:read). Any staff role
// with support:read (staff:owner, staff:devops) can read it; customer roles
// don't carry support:read and get 403.
func glossaryHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !can(claims, "support:read") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Static reference content — safe to cache briefly in the browser.
		w.Header().Set("Cache-Control", "private, max-age=300")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"count":  glossary.Count(),
			"groups": glossary.Grouped(),
		}); err != nil {
			log.Error("glossary encode failed", "error", err)
		}
	}
}

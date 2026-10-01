package main

// account_closure.go — account closure state machine (PLAN Phase 11, item 105:
// Account Closure and Data Export).
//
//	GET  /v1/api/account/close         — the caller's active closure status (or null)
//	POST /v1/api/account/close         — initiate a 30-day grace closure (owner-only)
//	POST /v1/api/account/close/cancel  — cancel an in-grace closure (owner-only)
//
// Initiating suspends the account and pauses its campaigns / deactivates its
// placements (no new spend, no new auctions) but leaves the owner able to sign
// in and cancel during the grace window — the pause is exactly reversible.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/accountlifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// closureDeps is what the closure handlers need: the lifecycle store plus the
// raw DB for audit writes. Either may be nil when Postgres is unavailable.
type closureDeps struct {
	store accountlifecycle.Store
	db    *sql.DB
}

// closureStatusResponse wraps the active closure (nil when none) for the portal.
type closureStatusResponse struct {
	Closure *accountlifecycle.ClosureRequest `json:"closure"`
}

func accountClosureHandler(deps *closureDeps, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		accountID, ok := effectiveAccount(r, claims)
		if !ok {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if devTenantGuard(w, r, accountID, closureStatusResponse{}) {
			return
		}
		if deps == nil || deps.store == nil {
			http.Error(w, `{"error":"account closure unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		if !canAs(r, claims, "account:close") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}

		switch r.Method {
		case http.MethodGet:
			active, err := deps.store.ActiveClosure(r.Context(), accountID)
			if err != nil {
				log.Error("account closure status failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(closureStatusResponse{Closure: active})

		case http.MethodPost:
			var req struct {
				Reason string `json:"reason,omitempty"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req) // reason optional
			cr, err := deps.store.RequestClosure(r.Context(), accountID, claims.UserID, req.Reason, accountlifecycle.DefaultGraceDays)
			if err != nil {
				if errors.Is(err, accountlifecycle.ErrAlreadyClosing) {
					http.Error(w, `{"error":"a closure is already in progress"}`, http.StatusConflict)
					return
				}
				log.Error("account closure request failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = audit.Log(r.Context(), deps.db, audit.Entry{
				AccountID:    accountID,
				ActorID:      "user:" + claims.UserID,
				Action:       "account:close",
				ResourceType: "account",
				ResourceID:   accountID,
				Changes: map[string]any{
					"grace_ends_at":          cr.GraceEndsAt,
					"paused_line_items":      len(cr.PausedLineItems),
					"deactivated_placements": len(cr.DeactivatedPlacements),
				},
				Reason: req.Reason,
			})
			log.Info("account closure initiated", "account", accountID, "grace_ends_at", cr.GraceEndsAt)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(cr)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

func accountCloseCancelHandler(deps *closureDeps, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		accountID, ok := effectiveAccount(r, claims)
		if !ok {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if devTenantGuard(w, r, accountID, map[string]string{}) {
			return
		}
		if deps == nil || deps.store == nil {
			http.Error(w, `{"error":"account closure unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !canAs(r, claims, "account:close") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		cr, err := deps.store.CancelClosure(r.Context(), accountID)
		if err != nil {
			if errors.Is(err, accountlifecycle.ErrNotClosing) {
				http.Error(w, `{"error":"no closure in progress"}`, http.StatusNotFound)
				return
			}
			log.Error("account closure cancel failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		_ = audit.Log(r.Context(), deps.db, audit.Entry{
			AccountID:    accountID,
			ActorID:      "user:" + claims.UserID,
			Action:       "account:close:cancel",
			ResourceType: "account",
			ResourceID:   accountID,
		})
		log.Info("account closure cancelled", "account", accountID)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(cr)
	}
}

package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/notifications"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// notificationsListResponse is the bell payload: the recent notifications plus
// the unread badge count, so one GET drives both the dropdown and the badge.
type notificationsListResponse struct {
	Notifications []notifications.Notification `json:"notifications"`
	Unread        int                          `json:"unread"`
}

// notificationsReadRequest marks read: either a single {id} or {"all":true}.
type notificationsReadRequest struct {
	ID  string `json:"id"`
	All bool   `json:"all"`
}

// notificationListLimit caps how many rows the dropdown fetches.
const notificationListLimit = 30

// notificationsHandler serves the per-account in-app notification feed for the
// portals:
//
//	GET  /v1/api/notifications      — recent notifications + unread count
//	POST /v1/api/notifications/read — {id} marks one read, {"all":true} marks all
//
// Tenant sessions only: every query is scoped to claims.AccountID, so a caller
// can never read or mutate another tenant's notifications. No feature
// permission is required beyond a valid session — every portal user sees their
// own account's alerts.
func notificationsHandler(store notifications.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, notificationsListResponse{Notifications: []notifications.Notification{}}) {
			return
		}
		if store == nil {
			http.Error(w, `{"error":"notification store unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		// The read sub-path (POST /v1/api/notifications/read) is the only thing
		// under the collection route; everything else is the collection itself.
		isRead := strings.TrimPrefix(r.URL.Path, routes.APINotifications) == "/read"

		switch {
		case r.Method == http.MethodGet && !isRead:
			list, err := store.ListForAccount(r.Context(), claims.AccountID, notificationListLimit)
			if err != nil {
				log.Error("notification list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			unread, err := store.UnreadCount(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("notification unread count failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(notificationsListResponse{Notifications: list, Unread: unread})

		case r.Method == http.MethodPost && isRead:
			var req notificationsReadRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
				return
			}
			if req.All {
				if err := store.MarkAllRead(r.Context(), claims.AccountID); err != nil {
					log.Error("notification mark-all-read failed", "error", err)
					http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
				return
			}
			id := strings.TrimSpace(req.ID)
			if id == "" {
				http.Error(w, `{"error":"id or all is required"}`, http.StatusBadRequest)
				return
			}
			err := store.MarkRead(r.Context(), claims.AccountID, id)
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, `{"error":"notification not found"}`, http.StatusNotFound)
				return
			}
			if err != nil {
				log.Error("notification mark-read failed", "id", id, "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "id": id})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

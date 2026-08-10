package main

// support.go — customer support & dispute resolution (PLAN Phase 11, item 108).
//
// Customer (support:contact, tenant-scoped to their own account):
//	GET  /v1/api/support/tickets                 — my tickets
//	POST /v1/api/support/tickets                 — open a ticket / dispute
//	GET  /v1/api/support/tickets/{id}            — my ticket + thread
//	POST /v1/api/support/tickets/{id}/messages   — reply
//
// Staff (support:read / support:update, cross-tenant via the platform hatch):
//	GET  /v1/api/support/tickets?scope=all       — the queue
//	GET  /v1/api/support/tickets/{id}            — any ticket + thread
//	POST /v1/api/support/tickets/{id}/messages   — staff reply
//	POST /v1/api/support/tickets/{id}/resolve    — resolve (+ credit a dispute)

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/support"
)

func isStaff(c *auth.Claims) bool { return c.AccountType == auth.AccountStaff }

// supportTicketsHandler serves the collection: GET list + POST create.
func supportTicketsHandler(store support.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, []support.Ticket{}) {
			return
		}
		if store == nil {
			http.Error(w, `{"error":"support unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		switch r.Method {
		case http.MethodGet:
			// Staff queue (all tenants) vs a customer's own list.
			if isStaff(claims) && r.URL.Query().Get("scope") == "all" {
				if !can(claims, "support:read") {
					http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
					return
				}
				tickets, err := store.ListAll(r.Context(), 200)
				if err != nil {
					log.Error("support list all failed", "error", err)
					http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
					return
				}
				_ = json.NewEncoder(w).Encode(tickets)
				return
			}
			if !can(claims, "support:contact") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			tickets, err := store.ListByAccount(r.Context(), claims.AccountID, 100)
			if err != nil {
				log.Error("support list mine failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			support.RedactListForCustomer(tickets) // never expose staff identity to a customer
			_ = json.NewEncoder(w).Encode(tickets)

		case http.MethodPost:
			if !can(claims, "support:contact") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			handleSupportCreate(w, r, store, claims, log)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

func handleSupportCreate(w http.ResponseWriter, r *http.Request, store support.Store, claims *auth.Claims, log *slog.Logger) {
	var req struct {
		Kind           string  `json:"kind"`
		Subject        string  `json:"subject"`
		Body           string  `json:"body"`
		AmountDisputed float64 `json:"amount_disputed"` // dollars (billing_dispute)
		Currency       string  `json:"currency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	req.Kind = strings.TrimSpace(req.Kind)
	req.Subject = strings.TrimSpace(req.Subject)
	if req.Kind == "" {
		req.Kind = support.KindQuestion
	}
	if !support.IsValidKind(req.Kind) {
		http.Error(w, `{"error":"kind must be question, technical or billing_dispute"}`, http.StatusBadRequest)
		return
	}
	if req.Subject == "" {
		http.Error(w, `{"error":"subject is required"}`, http.StatusBadRequest)
		return
	}
	t := support.Ticket{
		AccountID: claims.AccountID,
		Kind:      req.Kind,
		Subject:   req.Subject,
		Currency:  req.Currency,
		CreatedBy: claims.UserID,
	}
	if req.Kind == support.KindBillingDispute && req.AmountDisputed > 0 {
		micros := int64(req.AmountDisputed*1e6 + 0.5)
		t.AmountDisputedMicros = &micros
	}
	id, err := store.Create(r.Context(), t, strings.TrimSpace(req.Body))
	if err != nil {
		log.Error("support create failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	log.Info("support ticket opened", "id", id, "account", claims.AccountID, "kind", req.Kind)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
}

// supportTicketActionHandler serves the per-ticket subtree: detail, reply, resolve.
func supportTicketActionHandler(store support.Store, gwDB *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, support.Ticket{}) {
			return
		}
		if store == nil {
			http.Error(w, `{"error":"support unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, routes.APISupportTicketsSub)
		parts := strings.Split(strings.Trim(rest, "/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		ticketID := parts[0]
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}

		switch {
		case action == "" && r.Method == http.MethodGet:
			supportGetDetail(w, r, store, claims, ticketID, log)
		case action == "messages" && r.Method == http.MethodPost:
			supportReply(w, r, store, claims, ticketID, log)
		case action == "resolve" && r.Method == http.MethodPost:
			supportResolve(w, r, store, gwDB, claims, ticketID, log)
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}
}

func supportGetDetail(w http.ResponseWriter, r *http.Request, store support.Store, claims *auth.Claims, ticketID string, log *slog.Logger) {
	var t *support.Ticket
	var err error
	if isStaff(claims) {
		if !can(claims, "support:read") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		t, err = store.GetAny(r.Context(), ticketID)
	} else {
		if !can(claims, "support:contact") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		t, err = store.GetForAccount(r.Context(), claims.AccountID, ticketID)
	}
	if err != nil {
		log.Error("support get failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if t == nil {
		http.Error(w, `{"error":"ticket not found"}`, http.StatusNotFound)
		return
	}
	if !isStaff(claims) {
		support.RedactForCustomer(t) // hide staff identity on the customer's view
	}
	_ = json.NewEncoder(w).Encode(t)
}

func supportReply(w http.ResponseWriter, r *http.Request, store support.Store, claims *auth.Claims, ticketID string, log *slog.Logger) {
	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Body) == "" {
		http.Error(w, `{"error":"body is required"}`, http.StatusBadRequest)
		return
	}
	var err error
	if isStaff(claims) {
		if !can(claims, "support:update") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		err = store.AddStaffMessage(r.Context(), ticketID, claims.UserID, strings.TrimSpace(req.Body))
	} else {
		if !can(claims, "support:contact") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		err = store.AddCustomerMessage(r.Context(), claims.AccountID, ticketID, claims.UserID, strings.TrimSpace(req.Body))
	}
	if err != nil {
		log.Error("support reply failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// supportResolve is staff-only: sets the ticket's status/resolution and, for a
// billing dispute, optionally issues a credit adjustment to the account.
func supportResolve(w http.ResponseWriter, r *http.Request, store support.Store, gwDB *sql.DB, claims *auth.Claims, ticketID string, log *slog.Logger) {
	// Resolve is a cross-tenant (platform-hatch) write. Require BOTH the staff
	// account type AND support:update — defence in depth, so it can never be
	// reached by a non-staff principal even if the perm map changes.
	if !isStaff(claims) || !can(claims, "support:update") {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	var req struct {
		Status       string  `json:"status"`
		Resolution   string  `json:"resolution"`
		CreditAmount float64 `json:"credit_amount"` // dollars; issues an adjustment when > 0
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.Status == "" {
		req.Status = support.StatusResolved
	}
	if req.Status != support.StatusResolved && req.Status != support.StatusClosed &&
		req.Status != support.StatusPending && req.Status != support.StatusOpen {
		http.Error(w, `{"error":"status must be open, pending, resolved or closed"}`, http.StatusBadRequest)
		return
	}

	t, err := store.Resolve(r.Context(), ticketID, req.Status, strings.TrimSpace(req.Resolution), claims.UserID)
	if err != nil {
		log.Error("support resolve failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if t == nil {
		http.Error(w, `{"error":"ticket not found"}`, http.StatusNotFound)
		return
	}

	// A billing dispute can resolve with a credit — recorded in the canonical
	// adjustments table (manual credit/debit for disputes/refunds/errors).
	credited := false
	if req.CreditAmount > 0 && t.Kind == support.KindBillingDispute && gwDB != nil {
		// adjustments has RLS (mig 017): set the tenant GUC to the ticket's account
		// so the INSERT's WITH CHECK is satisfied (scoped, not a bare pool write).
		if err := insertAdjustmentTx(r.Context(), gwDB, t.AccountID, req.CreditAmount, t.Currency,
			"support dispute "+ticketID+": "+req.Resolution); err != nil {
			log.Error("support dispute credit adjustment failed", "ticket", ticketID, "error", err)
			http.Error(w, `{"error":"resolved, but credit adjustment failed"}`, http.StatusInternalServerError)
			return
		}
		credited = true
	}

	_ = audit.Log(r.Context(), gwDB, audit.Entry{
		AccountID:    t.AccountID,
		ActorID:      "user:" + claims.UserID,
		Action:       "support:resolve",
		ResourceType: "support_ticket",
		ResourceID:   ticketID,
		Changes:      map[string]any{"status": req.Status, "credit_amount": req.CreditAmount, "credited": credited},
		Reason:       req.Resolution,
	})
	log.Info("support ticket resolved", "ticket", ticketID, "status", req.Status, "credited", credited)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"ticket": t, "credited": credited})
}

// insertAdjustmentTx writes a credit adjustment under the account's tenant GUC
// so the RLS WITH CHECK on the adjustments table is satisfied.
func insertAdjustmentTx(ctx context.Context, db *sql.DB, accountID string, amount float64, currency, reason string) error {
	if currency == "" {
		currency = "USD"
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO adjustments (account_id, amount, currency, type, reason)
VALUES ($1::uuid, $2, $3, 'credit', $4)`, accountID, amount, currency, reason); err != nil {
		return err
	}
	return tx.Commit()
}

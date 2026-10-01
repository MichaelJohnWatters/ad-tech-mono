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

// Server-side bounds on customer free-text so a ticket/reply can't be an
// unbounded persisted TEXT blob (storage-amplification / memory DoS). The byte
// cap guards the whole request body before decode; the field caps give a clear
// 400 rather than a silent truncation.
const (
	maxSupportBodyBytes = 64 << 10 // 64 KiB whole-request cap
	maxSubjectLen       = 200
	maxMessageLen       = 16 << 10 // 16 KiB per message/resolution
)

// maxDisputeCreditMicros is an absolute per-adjustment ceiling on a staff dispute
// credit ($100k). A dispute credit is real money drawn against the platform, so
// it is additionally bounded by the amount the customer actually disputed (when
// stated) — this const is the backstop for disputes with no stated amount and a
// tripwire against a fat-fingered/compromised staff credit.
const maxDisputeCreditMicros int64 = 100_000 * 1_000_000

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
		// The effective account scopes the CUSTOMER path (a staff user "viewing
		// as" an advertiser/agency sees that account's own tickets). The staff
		// cross-tenant queue (scope=all / GetAny) below is unaffected — it keys
		// off isStaff(claims) and the platform hatch, not this account.
		accountID, ok := effectiveAccount(r, claims)
		if !ok {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if devTenantGuard(w, r, accountID, []support.Ticket{}) {
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
			if !canAs(r, claims, "support:contact") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			tickets, err := store.ListByAccount(r.Context(), accountID, 100)
			if err != nil {
				log.Error("support list mine failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			support.RedactListForCustomer(tickets) // never expose staff identity to a customer
			_ = json.NewEncoder(w).Encode(tickets)

		case http.MethodPost:
			if !canAs(r, claims, "support:contact") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			handleSupportCreate(w, r, store, claims, accountID, log)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

func handleSupportCreate(w http.ResponseWriter, r *http.Request, store support.Store, claims *auth.Claims, accountID string, log *slog.Logger) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSupportBodyBytes)
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
	if len(req.Subject) > maxSubjectLen {
		http.Error(w, `{"error":"subject too long"}`, http.StatusBadRequest)
		return
	}
	if len(req.Body) > maxMessageLen {
		http.Error(w, `{"error":"body too long"}`, http.StatusBadRequest)
		return
	}
	t := support.Ticket{
		AccountID: accountID,
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
	log.Info("support ticket opened", "id", id, "account", accountID, "kind", req.Kind)
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
		// Effective account scopes the CUSTOMER branches (GetForAccount /
		// AddCustomerMessage) when the caller is impersonating; the staff
		// (isStaff) branches stay cross-tenant via the platform hatch.
		accountID, ok := effectiveAccount(r, claims)
		if !ok {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if devTenantGuard(w, r, accountID, support.Ticket{}) {
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
			supportGetDetail(w, r, store, claims, accountID, ticketID, log)
		case action == "messages" && r.Method == http.MethodPost:
			supportReply(w, r, store, claims, accountID, ticketID, log)
		case action == "resolve" && r.Method == http.MethodPost:
			supportResolve(w, r, store, gwDB, claims, ticketID, log)
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}
}

func supportGetDetail(w http.ResponseWriter, r *http.Request, store support.Store, claims *auth.Claims, accountID, ticketID string, log *slog.Logger) {
	var t *support.Ticket
	var err error
	if isStaff(claims) {
		if !can(claims, "support:read") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		t, err = store.GetAny(r.Context(), ticketID)
	} else {
		if !canAs(r, claims, "support:contact") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		t, err = store.GetForAccount(r.Context(), accountID, ticketID)
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

func supportReply(w http.ResponseWriter, r *http.Request, store support.Store, claims *auth.Claims, accountID, ticketID string, log *slog.Logger) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSupportBodyBytes)
	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Body) == "" {
		http.Error(w, `{"error":"body is required"}`, http.StatusBadRequest)
		return
	}
	if len(req.Body) > maxMessageLen {
		http.Error(w, `{"error":"body too long"}`, http.StatusBadRequest)
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
		if !canAs(r, claims, "support:contact") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		err = store.AddCustomerMessage(r.Context(), accountID, ticketID, claims.UserID, strings.TrimSpace(req.Body))
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
	r.Body = http.MaxBytesReader(w, r.Body, maxSupportBodyBytes)
	var req struct {
		Status       string  `json:"status"`
		Resolution   string  `json:"resolution"`
		CreditAmount float64 `json:"credit_amount"` // dollars; issues an adjustment when > 0
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if len(req.Resolution) > maxMessageLen {
		http.Error(w, `{"error":"resolution too long"}`, http.StatusBadRequest)
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
	// Hard ceiling on a single dispute credit (backstop against a fat-fingered or
	// compromised staff credit); the tighter per-ticket bound is applied below.
	creditMicros := int64(req.CreditAmount*1e6 + 0.5)
	if req.CreditAmount > 0 && creditMicros > maxDisputeCreditMicros {
		http.Error(w, `{"error":"credit_amount exceeds the maximum permitted per adjustment"}`, http.StatusBadRequest)
		return
	}

	// Double-credit guard: a billing-dispute credit may be issued only on the
	// FIRST transition out of an open/pending state. Read the ticket's prior
	// status before resolving; re-resolving an already-resolved/closed ticket (a
	// double-submit or retry) must not insert a second adjustment (real money).
	prior, err := store.GetAny(r.Context(), ticketID)
	if err != nil {
		log.Error("support resolve: prior read failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if prior == nil {
		http.Error(w, `{"error":"ticket not found"}`, http.StatusNotFound)
		return
	}
	// A dispute credit cannot exceed what the customer actually disputed (when a
	// disputed amount was stated). Checked BEFORE resolving so we never leave a
	// resolved-but-uncredited ticket on rejection.
	if req.CreditAmount > 0 && prior.AmountDisputedMicros != nil && creditMicros > *prior.AmountDisputedMicros {
		http.Error(w, `{"error":"credit_amount cannot exceed the amount disputed on the ticket"}`, http.StatusBadRequest)
		return
	}
	alreadyClosed := prior.Status == support.StatusResolved || prior.Status == support.StatusClosed

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
	// adjustments table (manual credit/debit for disputes/refunds/errors). Skip
	// if the ticket was already resolved/closed (guards against double-crediting).
	credited := false
	if req.CreditAmount > 0 && t.Kind == support.KindBillingDispute && !alreadyClosed && gwDB != nil {
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

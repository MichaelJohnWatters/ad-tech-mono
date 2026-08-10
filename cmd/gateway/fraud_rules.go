package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// blocklistEntry is one fraud_blocklists row as the staff console sees it.
type blocklistEntry struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

type fraudRuleStore interface {
	ListBlocklist(ctx context.Context) ([]blocklistEntry, error)
	// AddBlocklist inserts (or upserts on the unique type+value index) an entry
	// and returns its id.
	AddBlocklist(ctx context.Context, typ, value, reason string) (id string, err error)
	// DeleteBlocklist removes an entry; returns sql.ErrNoRows if id is unknown.
	DeleteBlocklist(ctx context.Context, id string) error
}

// validBlocklistTypes mirrors the fraud_blocklists.type CHECK constraint.
var validBlocklistTypes = map[string]bool{"ip": true, "ua": true, "domain": true, "app_bundle": true}

// fraudRulesHandler manages the fraud blocklist: GET lists (fraud:read), POST
// adds and DELETE removes (fraud:update). Platform-wide — fraud_blocklists has
// no account_id, so it's permission-gated, not tenant-scoped. Mutations publish
// the fraud-rules cache invalidate so the tracker's warm cache reloads
// sub-second.
func fraudRulesHandler(store fraudRuleStore, bus events.EventBus, auditDB *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "fraud:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			entries, err := store.ListBlocklist(r.Context())
			if err != nil {
				log.Error("fraud blocklist list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(entries)

		case http.MethodPost:
			if !can(claims, "fraud:update") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var in struct {
				Type   string `json:"type"`
				Value  string `json:"value"`
				Reason string `json:"reason"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
				return
			}
			in.Type = strings.TrimSpace(in.Type)
			in.Value = strings.TrimSpace(in.Value)
			if !validBlocklistTypes[in.Type] {
				http.Error(w, `{"error":"type must be ip, ua, domain or app_bundle"}`, http.StatusBadRequest)
				return
			}
			if in.Value == "" {
				http.Error(w, `{"error":"value required"}`, http.StatusBadRequest)
				return
			}
			id, err := store.AddBlocklist(r.Context(), in.Type, in.Value, in.Reason)
			if err != nil {
				log.Error("fraud blocklist add failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			publishFraudInvalidate(r.Context(), bus, id)
			// Platform-wide traffic control (affects every tenant's bidding) —
			// audit who blocked what.
			_ = audit.Log(r.Context(), auditDB, audit.Entry{
				ActorID:      "user:" + claims.UserID,
				Action:       "fraud:blocklist_add",
				ResourceType: "fraud_blocklist",
				ResourceID:   id,
				Changes:      map[string]any{"type": in.Type, "value": in.Value, "reason": in.Reason},
			})
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "type": in.Type, "value": in.Value})

		case http.MethodDelete:
			if !can(claims, "fraud:update") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			id := strings.TrimSpace(r.URL.Query().Get("id"))
			if id == "" {
				http.Error(w, `{"error":"id query param required"}`, http.StatusBadRequest)
				return
			}
			err := store.DeleteBlocklist(r.Context(), id)
			if err == sql.ErrNoRows {
				http.Error(w, `{"error":"blocklist entry not found"}`, http.StatusNotFound)
				return
			}
			if err != nil {
				log.Error("fraud blocklist delete failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			publishFraudInvalidate(r.Context(), bus, id)
			_ = audit.Log(r.Context(), auditDB, audit.Entry{
				ActorID:      "user:" + claims.UserID,
				Action:       "fraud:blocklist_delete",
				ResourceType: "fraud_blocklist",
				ResourceID:   id,
			})
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "deleted"})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

func publishFraudInvalidate(ctx context.Context, bus events.EventBus, id string) {
	if bus == nil {
		return
	}
	_ = bus.Publish(ctx, events.SubjectCacheInvalidateFraudRules, []byte(`{"source":"gateway","id":"`+id+`"}`))
}

type pgFraudRuleStore struct{ db *sql.DB }

func (s pgFraudRuleStore) ListBlocklist(ctx context.Context) ([]blocklistEntry, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id::text, type, value, COALESCE(reason,'') FROM fraud_blocklists ORDER BY created_at DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []blocklistEntry{}
	for rows.Next() {
		var e blocklistEntry
		if err := rows.Scan(&e.ID, &e.Type, &e.Value, &e.Reason); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s pgFraudRuleStore) AddBlocklist(ctx context.Context, typ, value, reason string) (string, error) {
	if s.db == nil {
		return "", sql.ErrConnDone
	}
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	var id string
	// Upsert on the unique (type, value) index — re-adding an entry refreshes its
	// reason rather than erroring.
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO fraud_blocklists (type, value, reason) VALUES ($1, $2, $3)
		 ON CONFLICT (type, value) DO UPDATE SET reason = EXCLUDED.reason
		 RETURNING id::text`,
		typ, value, reasonArg).Scan(&id)
	return id, err
}

func (s pgFraudRuleStore) DeleteBlocklist(ctx context.Context, id string) error {
	if s.db == nil {
		return sql.ErrConnDone
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM fraud_blocklists WHERE id = $1::uuid`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// publisherCreateInput is a publisher onboarding its site — the missing
// piece between signup (which only creates the account) and everything
// else on the supply side (placements, deals, quality lists, ad tags all
// hang off a publishers row).
type publisherCreateInput struct {
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

type publisherCreateStore interface {
	CreatePublisher(ctx context.Context, accountID string, in publisherCreateInput) (id string, err error)
}

// publisherCreateHandler serves POST /v1/api/publishers (placements:create):
// creates a publishers row under the caller's account with the standard
// rev-share defaults, active immediately (dev posture — a review flow can
// flip the default to 'pending' later). GETs never reach here — the route
// dispatcher sends them to the SSP list proxy.
func publisherCreateHandler(store publisherCreateStore, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
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
		if devTenantGuard(w, r, accountID, nil) {
			return
		}
		if !canAs(r, claims, "placements:create") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		var in publisherCreateInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		in.Domain = strings.TrimSpace(strings.ToLower(in.Domain))
		if in.Name == "" || in.Domain == "" {
			http.Error(w, `{"error":"name and domain are required"}`, http.StatusBadRequest)
			return
		}
		id, err := store.CreatePublisher(r.Context(), accountID, in)
		if err != nil {
			log.Error("publisher create failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if bus != nil {
			// Publishers feed two warm caches: the SSP placement join and the
			// billing engine's rev-share contracts.
			payload := []byte(`{"source":"gateway","id":"` + id + `"}`)
			_ = bus.Publish(r.Context(), events.SubjectCacheInvalidatePublishers, payload)
			_ = bus.Publish(r.Context(), events.SubjectCacheInvalidateBillingRates, payload)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "account_id": accountID, "status": "active"})
	}
}

type pgPublisherCreateStore struct{ db *sql.DB }

func (s pgPublisherCreateStore) CreatePublisher(ctx context.Context, accountID string, in publisherCreateInput) (string, error) {
	if s.db == nil {
		return "", sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, accountID); err != nil {
		return "", fmt.Errorf("set tenant: %w", err)
	}
	// Schema defaults supply the standard rev-share contract (fixed 20%).
	var id string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO publishers (account_id, name, domain, status, created_at, updated_at)
		 VALUES ($1::uuid, $2, $3, 'active', now(), now()) RETURNING id::text`,
		accountID, in.Name, in.Domain).Scan(&id); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

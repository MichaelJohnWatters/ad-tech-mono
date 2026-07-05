package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
)

// sellerSource yields the active publishers the platform represents, for the
// IAB sellers.json endpoint. Backed by the publishers table so adding a
// publisher changes the output with no code change.
type sellerSource interface {
	Sellers(ctx context.Context) ([]fraud.SellerEntry, error)
}

// sellersJSONHandler serves /sellers.json (IAB Tech Lab sellers.json spec) from
// the live publisher list. If the source is unavailable it serves a valid file
// with an empty seller list rather than a stale hardcoded set — better to
// declare "no sellers right now" than to claim publishers we can't confirm.
func sellersJSONHandler(src sellerSource, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sellers, err := src.Sellers(r.Context())
		if err != nil {
			log.Warn("sellers.json: publisher query failed, serving empty seller list", "error", err)
			sellers = []fraud.SellerEntry{}
		}
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(fraud.GenerateSellersJSON(sellers))
	}
}

// pgSellerStore reads active publishers. Like ContractLoader, this is a
// platform-wide (cross-tenant) read: the app role owns the publishers table, so
// RLS does not filter it, and sellers.json is inherently a whole-platform view.
type pgSellerStore struct{ db *sql.DB }

func (s pgSellerStore) Sellers(ctx context.Context) ([]fraud.SellerEntry, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id::text, name, domain FROM publishers WHERE status = 'active' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []fraud.SellerEntry{}
	for rows.Next() {
		var id, name, domain string
		if err := rows.Scan(&id, &name, &domain); err != nil {
			return nil, err
		}
		out = append(out, fraud.SellerEntry{
			SellerID:   id,
			Name:       name,
			Domain:     domain,
			SellerType: "PUBLISHER",
		})
	}
	return out, rows.Err()
}

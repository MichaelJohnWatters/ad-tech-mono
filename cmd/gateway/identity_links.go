package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// identityLinkStore is the write dependency of the ingestion handler — the
// interface keeps the handler unit-testable without a real Postgres.
type identityLinkStore interface {
	LinkIdentity(ctx context.Context, edges []postgres.IdentityEdge) (int, error)
}

// identityLinkRequest ingests identity-graph edges. Two shapes are accepted:
//
//   - UID2-centric (the common case): a uid2 token plus the identifiers it maps
//     to. Each becomes an edge {user_id: uid2, linked_id: <id>, source: uid2}.
//   - Generic: an explicit list of fully-specified edges.
//
// A request may use either or both. account_id isn't stored — identity_graph is
// platform-global (cross-tenant by design).
type identityLinkRequest struct {
	UID2  string           `json:"uid2,omitempty"`
	Links []identityLinkID `json:"links,omitempty"` // partners for the uid2
	Edges []identityEdgeIn `json:"edges,omitempty"` // fully-specified edges
}

// identityLinkID is one identifier a UID2 maps to.
type identityLinkID struct {
	ID         string  `json:"id"`
	Source     string  `json:"source,omitempty"`     // default hashed_email
	LinkType   string  `json:"link_type,omitempty"`  // default cross_device
	Confidence float64 `json:"confidence,omitempty"` // default 1.0
}

// identityEdgeIn is a fully-specified edge.
type identityEdgeIn struct {
	UserID     string  `json:"user_id"`
	LinkedID   string  `json:"linked_id"`
	Source     string  `json:"source,omitempty"`
	LinkType   string  `json:"link_type,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

type identityLinkResponse struct {
	LinksWritten int `json:"links_written"`
}

// identityLinksHandler handles POST /v1/api/identity-links. Writes the edges to
// the identity graph and reports how many landed.
func identityLinksHandler(store identityLinkStore, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			http.Error(w, "identity store unavailable", http.StatusServiceUnavailable)
			return
		}
		var req identityLinkRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		edges := make([]postgres.IdentityEdge, 0, len(req.Links)+len(req.Edges))
		for _, l := range req.Links {
			if req.UID2 == "" || l.ID == "" {
				continue
			}
			edges = append(edges, postgres.IdentityEdge{
				UserID:     req.UID2,
				LinkedID:   l.ID,
				Source:     defaultStr(l.Source, identity.SourceUID2),
				LinkType:   defaultStr(l.LinkType, identity.LinkCrossDevice),
				Confidence: l.Confidence,
			})
		}
		for _, e := range req.Edges {
			if e.UserID == "" || e.LinkedID == "" {
				continue
			}
			edges = append(edges, postgres.IdentityEdge{
				UserID:     e.UserID,
				LinkedID:   e.LinkedID,
				Source:     defaultStr(e.Source, identity.SourceHashedEmail),
				LinkType:   defaultStr(e.LinkType, identity.LinkCRMMatch),
				Confidence: e.Confidence,
			})
		}
		if len(edges) == 0 {
			http.Error(w, "no valid edges (need uid2+links or edges)", http.StatusBadRequest)
			return
		}

		n, err := store.LinkIdentity(r.Context(), edges)
		if err != nil {
			log.Error("identity link ingest failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		log.Info("identity links ingested", "written", n, "submitted", len(edges))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(identityLinkResponse{LinksWritten: n})
	}
}

// defaultStr returns v when non-empty, else dflt.
func defaultStr(v, dflt string) string {
	if v == "" {
		return dflt
	}
	return v
}

package main

// profiles.go — the staff profile-transparency API (profile store payoff
// valve): GET /v1/api/profiles/{id} answers "what does the platform know
// about this identifier" in one place, trace-explorer style:
//
//   - person: the materialized identity cluster (identity_clusters, built by
//     cmd/profile-builder) the id belongs to, with every member id.
//   - identity_links: the id's direct identity_graph edges (pre-clustering
//     truth, includes household links the clusterer deliberately excludes).
//   - memberships: every audience segment any cluster member is in, with
//     provenance (segment source/visibility/account).
//   - lake: the pipeline's Delta-side summary — onboarding signal rows that
//     mention the id and behavioural row counts by kind.
//
// Staff-only (support:read): profiles span tenants by nature, exactly like
// the audit log. Also the GDPR "access request" answer surface.

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

type profileMembership struct {
	MemberID   string `json:"member_id"` // which cluster member holds it
	SegmentID  string `json:"segment_id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Visibility string `json:"visibility"`
	Source     string `json:"source"`
	AccountID  string `json:"account_id"`
}

type profileView struct {
	ID             string              `json:"id"`
	PersonID       string              `json:"person_id,omitempty"`
	ClusterMembers []string            `json:"cluster_members"`
	IdentityLinks  []string            `json:"identity_links"`
	Memberships    []profileMembership `json:"memberships"`
	Lake           json.RawMessage     `json:"lake,omitempty"` // pipeline lakeProfileSummary passthrough
	LakeError      string              `json:"lake_error,omitempty"`
}

// identityResolver is the direct-edges lookup (pkg/store/postgres.Store).
type identityResolver interface {
	ResolveIdentity(ctx context.Context, id string) ([]string, error)
}

func profilesHandler(db *sql.DB, resolver identityResolver, pipelineURL string, log *slog.Logger) http.HandlerFunc {
	client := &http.Client{Timeout: 20 * time.Second}
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
		if db == nil {
			http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/api/profiles"), "/")
		if id == "" {
			http.Error(w, `{"error":"profile id required: /v1/api/profiles/{id}"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ctx := r.Context()

		view, err := buildProfileView(ctx, db, resolver, id)
		if err != nil {
			log.Error("profile lookup failed", "id", id, "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}

		// Lake summary via the pipeline (it owns the Delta reader). Advisory:
		// a pipeline outage degrades the view, it doesn't 500 it.
		if pipelineURL != "" {
			u := pipelineURL + "/v1/datalake/profile?user_id=" + url.QueryEscape(id)
			if resp, err := client.Get(u); err != nil {
				view.LakeError = "pipeline unreachable"
				log.Warn("profile lookup: lake summary failed", "error", err)
			} else {
				func() {
					defer resp.Body.Close()
					var raw json.RawMessage
					if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&raw) == nil {
						view.Lake = raw
					} else {
						view.LakeError = resp.Status
					}
				}()
			}
		}

		_ = json.NewEncoder(w).Encode(view)
	}
}

// buildProfileView assembles the identity-cluster + graph-links + memberships
// snapshot for one identifier from Postgres. It's the shared read path behind
// the staff profiles API and the onboarding demo (BEFORE/AFTER snapshots) — the
// lake-summary passthrough stays in the handler because it needs the pipeline
// HTTP client. resolver may be nil (direct graph edges are then omitted). A DB
// error is returned to the caller; a per-row scan error is skipped, matching the
// handler's original best-effort posture.
func buildProfileView(ctx context.Context, db *sql.DB, resolver identityResolver, id string) (profileView, error) {
	view := profileView{ID: id, ClusterMembers: []string{id}, IdentityLinks: []string{}, Memberships: []profileMembership{}}
	if db == nil {
		return view, nil
	}

	// Cluster: the person this id belongs to, if the builder materialized one
	// (singletons have no row — the id is its own person).
	var personID string
	if err := db.QueryRowContext(ctx, `SELECT person_id FROM identity_clusters WHERE member_id = $1`, id).Scan(&personID); err != nil && err != sql.ErrNoRows {
		return view, err
	}
	if personID != "" {
		view.PersonID = personID
		rows, err := db.QueryContext(ctx, `SELECT member_id FROM identity_clusters WHERE person_id = $1 ORDER BY member_id`, personID)
		if err != nil {
			return view, err
		}
		view.ClusterMembers = view.ClusterMembers[:0]
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err == nil {
				view.ClusterMembers = append(view.ClusterMembers, m)
			}
		}
		rows.Close()
	}

	// Direct graph edges (includes household links the clusterer excludes).
	if resolver != nil {
		if links, err := resolver.ResolveIdentity(ctx, id); err == nil && links != nil {
			view.IdentityLinks = links
		}
	}

	// Memberships across the whole cluster, with provenance. Platform-wide read
	// (staff surface) — the same cross-tenant posture as the audit log.
	mrows, err := db.QueryContext(ctx, `
SELECT m.user_id, m.segment_id::text, s.name, s.type, s.visibility, COALESCE(s.source,''), s.account_id::text
FROM audience_segment_members m
JOIN audience_segments s ON s.id = m.segment_id
WHERE m.user_id = ANY($1)
ORDER BY s.name, m.user_id`, pq.Array(view.ClusterMembers))
	if err != nil {
		return view, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var m profileMembership
		if err := mrows.Scan(&m.MemberID, &m.SegmentID, &m.Name, &m.Type, &m.Visibility, &m.Source, &m.AccountID); err == nil {
			view.Memberships = append(view.Memberships, m)
		}
	}
	return view, mrows.Err()
}

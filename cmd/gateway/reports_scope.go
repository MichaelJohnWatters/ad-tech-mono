package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// reportTenantPublisherLookup lists the publisher IDs an account owns —
// what a publisher session's report queries may filter on.
type reportTenantPublisherLookup interface {
	PublisherIDs(ctx context.Context, accountID string) ([]string, error)
}

// enforceReportTenant rewrites the report-query body so a customer session
// can only query its own slice, no matter what filters the browser sent.
// The portal JS also sets these filters — this is the server-side guarantee
// that omitting them isn't a bypass.
//
//   - advertiser/agency: filters.account_id is forced to the session account.
//   - publisher: filters.publisher_id must be one of the account's publishers
//     (verified); if absent and the account owns exactly one, it's injected;
//     absent with several publishers is a 400 (the analytics filter model is
//     single-valued, so "all my publishers" can't be expressed upstream yet).
//   - staff/admin (and the dev bypass): untouched, platform-wide.
func enforceReportTenant(pubs reportTenantPublisherLookup, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := middleware.ClaimsFromContext(r.Context())
			if claims == nil || r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}

			// Effective tenant to scope report queries to. A customer session
			// scopes to its own account. Staff/admin are platform-wide (untouched)
			// UNLESS impersonating an account (act-as) — then scope to the target,
			// so an impersonated portal shows only that account's data (audited by
			// the proxy). A target the caller can't access is rejected.
			effType, effID := claims.AccountType, claims.AccountID
			if auth.IsPlatformUser(claims) {
				target := middleware.ActAsTarget(r)
				if target == "" {
					next.ServeHTTP(w, r)
					return
				}
				t, id := middleware.ParseActAsTarget(target)
				if !auth.CanAccessAccount(claims, id) {
					http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
					return
				}
				effType, effID = t, id
			}

			var q map[string]any
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil || json.Unmarshal(body, &q) != nil {
				http.Error(w, `{"error":"invalid query body"}`, http.StatusBadRequest)
				return
			}
			if q == nil {
				q = map[string]any{}
			}
			filters, _ := q["filters"].(map[string]any)
			if filters == nil {
				filters = map[string]any{}
			}

			switch effType {
			case auth.AccountAdvertiser, auth.AccountAgency:
				filters["account_id"] = effID

			case auth.AccountPublisher:
				owned, err := pubs.PublisherIDs(r.Context(), effID)
				if err != nil {
					log.Error("report tenant: publisher lookup failed", "error", err, "account_id", effID)
					http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
					return
				}
				ownedSet := map[string]bool{}
				for _, id := range owned {
					ownedSet[id] = true
				}
				if want, _ := filters["publisher_id"].(string); want != "" {
					if !ownedSet[want] {
						http.Error(w, `{"error":"forbidden: publisher not in your account"}`, http.StatusForbidden)
						return
					}
				} else if len(owned) == 1 {
					filters["publisher_id"] = owned[0]
				} else {
					http.Error(w, `{"error":"publisher_id filter required (your account has multiple publishers)"}`, http.StatusBadRequest)
					return
				}

			default:
				// Unknown customer type — refuse rather than leak platform-wide.
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}

			q["filters"] = filters
			rewritten, err := json.Marshal(q)
			if err != nil {
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(rewritten))
			r.ContentLength = int64(len(rewritten))
			next.ServeHTTP(w, r)
		})
	}
}

// injectTraceScope is the GET-route twin of enforceReportTenant for the trace
// inspector (/v1/api/trace, /v1/api/impressions/recent). Advertiser/agency scope
// rides through on X-Account-ID (the proxy injects it from claims/act-as), so
// nothing is needed here. For a publisher it validates the requested
// publisher_id against the account's owned publishers and sets the TRUSTED
// X-Publisher-ID header reporting reads — after stripping any client-supplied
// one, so a publisher can't forge another publisher's scope. Staff/admin (not
// impersonating) pass through unscoped.
func injectTraceScope(pubs reportTenantPublisherLookup, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del(constants.HeaderPublisherID) // never trust a client value
			claims := middleware.ClaimsFromContext(r.Context())
			if claims == nil {
				next.ServeHTTP(w, r)
				return
			}
			effType, effID := claims.AccountType, claims.AccountID
			if auth.IsPlatformUser(claims) {
				target := middleware.ActAsTarget(r)
				if target == "" {
					next.ServeHTTP(w, r) // staff/admin, unscoped
					return
				}
				t, id := middleware.ParseActAsTarget(target)
				if !auth.CanAccessAccount(claims, id) {
					http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
					return
				}
				effType, effID = t, id
			}
			switch effType {
			case auth.AccountAdvertiser, auth.AccountAgency:
				// Scoped by X-Account-ID downstream; nothing to inject.
			case auth.AccountPublisher:
				owned, err := pubs.PublisherIDs(r.Context(), effID)
				if err != nil {
					log.Error("trace scope: publisher lookup failed", "error", err, "account_id", effID)
					http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
					return
				}
				chosen := ""
				if want := r.URL.Query().Get("publisher_id"); want != "" {
					for _, id := range owned {
						if id == want {
							chosen = want
							break
						}
					}
					if chosen == "" {
						http.Error(w, `{"error":"forbidden: publisher not in your account"}`, http.StatusForbidden)
						return
					}
				} else if len(owned) == 1 {
					chosen = owned[0]
				} else if len(owned) == 0 {
					http.Error(w, `{"error":"no publishers on this account"}`, http.StatusForbidden)
					return
				} else {
					http.Error(w, `{"error":"publisher_id required (your account has multiple publishers)"}`, http.StatusBadRequest)
					return
				}
				r.Header.Set(constants.HeaderPublisherID, chosen)
			default:
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type pgPublisherLookup struct{ db *sql.DB }

func (s pgPublisherLookup) PublisherIDs(ctx context.Context, accountID string) ([]string, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id::text FROM publishers WHERE account_id = $1::uuid AND status != 'archived'`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

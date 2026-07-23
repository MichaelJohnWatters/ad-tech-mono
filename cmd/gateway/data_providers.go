package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/dataproviders"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// providerCreateRequest is the POST .../providers body — create/update a data
// provider. account_id is IGNORED; the handler binds to the JWT claims. Omitted
// enum fields are defaulted from kind (see dataproviders.Normalize), so the UI
// can send just {name, kind} and get a coherent provider back.
type providerCreateRequest struct {
	Name               string   `json:"name"`
	Kind               string   `json:"kind,omitempty"`
	DefaultParty       string   `json:"default_party,omitempty"`
	DefaultLicence     string   `json:"default_licence,omitempty"`
	DefaultIDType      string   `json:"default_id_type,omitempty"`
	EncryptionExpected bool     `json:"encryption_expected,omitempty"`
	NotifyEmails       []string `json:"notify_emails,omitempty"`
	Status             string   `json:"status,omitempty"`
}

// audienceProvidersHandler serves the tenant-scoped data-provider registry (ADR
// 0009):
//
//	GET    /v1/api/audiences/providers        — list the account's providers (audiences:read)
//	POST   /v1/api/audiences/providers        — create/update a provider (audiences:upload)
//	DELETE /v1/api/audiences/providers/{id}    — delete one (audiences:upload)
//
// All bind to the authenticated account from the JWT claims (RLS + explicit
// account_id filter); a caller can never read/write/delete another tenant's
// provider. Deleting a provider detaches (SET NULL) its ingest/segment history
// rather than cascading it away.
func audienceProvidersHandler(store dataproviders.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if store == nil {
			http.Error(w, `{"error":"provider store unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, routes.APIAudienceProviders), "/")

		switch r.Method {
		case http.MethodGet:
			if id != "" {
				if !can(claims, "audiences:read") {
					http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
					return
				}
				p, err := store.GetByAccount(r.Context(), claims.AccountID, id)
				if err != nil {
					log.Error("data provider get failed", "id", id, "error", err)
					http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
					return
				}
				if p == nil {
					http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(p)
				return
			}
			if !can(claims, "audiences:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			list, err := store.ListByAccount(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("data providers list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(list)

		case http.MethodPost:
			if id != "" {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			if !can(claims, "audiences:upload") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var req providerCreateRequest
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
				http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
				return
			}
			p := dataproviders.Provider{
				AccountID:          claims.AccountID, // never trust a body account_id
				Name:               req.Name,
				Kind:               req.Kind,
				DefaultParty:       req.DefaultParty,
				DefaultLicence:     req.DefaultLicence,
				DefaultIDType:      req.DefaultIDType,
				EncryptionExpected: req.EncryptionExpected,
				NotifyEmails:       req.NotifyEmails,
				Status:             req.Status,
			}
			dataproviders.Normalize(&p)
			if err := dataproviders.Validate(p); err != nil {
				http.Error(w, `{"error":`+jsonStr("invalid provider: "+err.Error())+`}`, http.StatusUnprocessableEntity)
				return
			}
			providerID, err := store.Create(r.Context(), p)
			if err != nil {
				log.Error("data provider create failed", "name", req.Name, "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": providerID})

		case http.MethodDelete:
			if id == "" || strings.Contains(id, "/") {
				http.Error(w, `{"error":"provider id required"}`, http.StatusBadRequest)
				return
			}
			if !can(claims, "audiences:upload") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			if err := store.DeleteByAccount(r.Context(), claims.AccountID, id); err != nil {
				log.Error("data provider delete failed", "id", id, "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

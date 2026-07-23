package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audiencemappings"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingest"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// mappingSampleResponse is the POST .../mappings/sample body: the sample's
// detected (lowercased) columns + a suggested source→canonical map for any
// column that looks like an id (ADR 0008). The UI pre-fills the builder from it.
type mappingSampleResponse struct {
	Columns   []string          `json:"columns"`
	Suggested map[string]string `json:"suggested"`
}

// mappingCreateRequest is the POST .../mappings body — create/update a saved
// mapping. account_id is IGNORED; the handler binds to the JWT claims.
type mappingCreateRequest struct {
	Name     string            `json:"name"`
	Mappings map[string]string `json:"mappings"`
	IDType   string            `json:"id_type,omitempty"`
}

// audienceMappingSampleHandler serves POST /v1/api/audiences/mappings/sample:
// accept a small multipart sample file, decode it with the strict parser, and
// return its lowercased columns + a suggested id_value mapping. Tenant-gated on
// audiences:upload (building a mapping is part of the upload flow). 422 when the
// sample can't be parsed.
func audienceMappingSampleHandler(maxBytes int, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !can(claims, "audiences:upload") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		// Sample uploads are small — cap at ~1MB regardless of the upload max.
		sampleMax := maxBytes
		if sampleMax <= 0 || sampleMax > 1<<20 {
			sampleMax = 1 << 20
		}
		r.Body = http.MaxBytesReader(w, r.Body, int64(sampleMax))
		if err := r.ParseMultipartForm(int64(sampleMax)); err != nil {
			http.Error(w, `{"error":"file too large or malformed multipart body"}`, http.StatusBadRequest)
			return
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, `{"error":"missing file field"}`, http.StatusBadRequest)
			return
		}
		defer file.Close()
		body, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, `{"error":"read file: `+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		name := "sample.csv"
		if hdr != nil && hdr.Filename != "" {
			name = hdr.Filename
		}
		records, err := ingest.DecodeFile(r.Context(), name, body)
		if err != nil {
			http.Error(w, `{"error":`+jsonStr("could not parse sample: "+err.Error())+`}`, http.StatusUnprocessableEntity)
			return
		}
		if len(records) == 0 {
			http.Error(w, `{"error":"sample has no data rows"}`, http.StatusUnprocessableEntity)
			return
		}
		// Columns come from the first row's keys, lowercased (decode already
		// lowercases headers). A map has no order — sort for a stable response.
		cols := make([]string, 0, len(records[0]))
		for c := range records[0] {
			cols = append(cols, strings.ToLower(c))
		}
		sort.Strings(cols)

		idLike := map[string]bool{}
		for _, c := range ingest.DefaultIDColumns() {
			idLike[c] = true
		}
		suggested := map[string]string{}
		for _, c := range cols {
			if idLike[c] {
				suggested[c] = "id_value"
			}
		}
		_ = json.NewEncoder(w).Encode(mappingSampleResponse{Columns: cols, Suggested: suggested})
	}
}

// audienceMappingsHandler serves the tenant-scoped mappings collection:
//
//	GET    /v1/api/audiences/mappings        — list the account's mappings (audiences:read)
//	POST   /v1/api/audiences/mappings        — create/update {name, mappings, id_type} (audiences:upload)
//	DELETE /v1/api/audiences/mappings/{id}    — delete one (audiences:upload)
//
// All bind to the authenticated account from the JWT claims (RLS + explicit
// account_id filter); a caller can never read/write/delete another tenant's
// mapping.
func audienceMappingsHandler(store audiencemappings.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if store == nil {
			http.Error(w, `{"error":"audience store unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		// Extract a trailing {id} if present (DELETE .../mappings/{id}).
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, routes.APIAudienceMappings), "/")

		switch r.Method {
		case http.MethodGet:
			if id != "" {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			if !can(claims, "audiences:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			list, err := store.ListByAccount(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("audience mappings list failed", "error", err)
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
			var req mappingCreateRequest
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
				http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
				return
			}
			req.Name = strings.TrimSpace(req.Name)
			if req.Name == "" {
				http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
				return
			}
			// Lowercase source columns so they match decode's lowercased headers.
			lowered := make(map[string]string, len(req.Mappings))
			for src, target := range req.Mappings {
				lowered[strings.ToLower(strings.TrimSpace(src))] = strings.TrimSpace(target)
			}
			m := audiencemappings.Mapping{
				AccountID: claims.AccountID, // never trust a body account_id
				Name:      req.Name,
				Mappings:  lowered,
				IDType:    req.IDType,
			}
			if err := audiencemappings.ValidateMapping(m); err != nil {
				http.Error(w, `{"error":`+jsonStr("invalid mapping: "+err.Error())+`}`, http.StatusUnprocessableEntity)
				return
			}
			mappingID, err := store.Create(r.Context(), m)
			if err != nil {
				log.Error("audience mapping create failed", "name", req.Name, "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": mappingID})

		case http.MethodDelete:
			if id == "" || strings.Contains(id, "/") {
				http.Error(w, `{"error":"mapping id required"}`, http.StatusBadRequest)
				return
			}
			if !can(claims, "audiences:upload") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			if err := store.DeleteByAccount(r.Context(), claims.AccountID, id); err != nil {
				log.Error("audience mapping delete failed", "id", id, "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

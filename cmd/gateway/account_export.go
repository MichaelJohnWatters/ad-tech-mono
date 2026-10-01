package main

// account_export.go — account data-export package (PLAN Phase 11, item 105,
// slice 2).
//
//	GET /v1/api/account/export           — enqueue (or return the status of) the export job
//	GET /v1/api/account/export/download  — stream the completed archive (private bucket egress)
//
// The archive is built asynchronously by the report-runner worker into the
// private adtech-reports bucket; the gateway is the only authenticated way it
// leaves the platform, so downloads STREAM through here (never a public URL).

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/accountexport"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
)

// accountExportView is the portal-facing status of the caller's export.
type accountExportView struct {
	Status        string     `json:"status"` // none|queued|running|done|failed
	ArtifactBytes int64      `json:"artifact_bytes,omitempty"`
	DownloadURL   string     `json:"download_url,omitempty"`
	Error         string     `json:"error,omitempty"`
	RequestedAt   *time.Time `json:"requested_at,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
}

func accountExportHandler(store accountexport.Store, objStore objects.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		// The download sub-route streams binary; only the status route is JSON.
		isDownload := strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/download")
		if !isDownload {
			w.Header().Set("Content-Type", "application/json")
		}
		accountID, ok := effectiveAccount(r, claims)
		if !ok {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if devTenantGuard(w, r, accountID, accountExportView{Status: "none"}) {
			return
		}
		if store == nil {
			http.Error(w, `{"error":"account export unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		if !canAs(r, claims, "account:export") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}

		switch {
		case isDownload && r.Method == http.MethodGet:
			accountExportDownload(w, r, store, objStore, accountID, log)

		case r.Method == http.MethodGet:
			// Status only — never enqueues (so merely opening the tab is cheap).
			job, err := store.LatestForAccount(r.Context(), accountID)
			if err != nil {
				log.Error("account export status failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			writeJSON(w, exportView(job, time.Now()), http.StatusOK)

		case r.Method == http.MethodPost:
			// Enqueue a fresh export, coalescing onto an in-flight job.
			job, err := store.Enqueue(r.Context(), accountID, claims.UserID, accountexport.DefaultTTL)
			if err != nil {
				log.Error("account export enqueue failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			log.Info("account export requested", "account", accountID, "job", job.ID)
			writeJSON(w, exportView(&job, time.Now()), http.StatusOK)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

func exportView(job *accountexport.Job, now time.Time) accountExportView {
	if job == nil {
		return accountExportView{Status: "none"}
	}
	v := accountExportView{
		Status:        job.Status,
		ArtifactBytes: job.ArtifactBytes,
		Error:         job.Error,
		RequestedAt:   &job.CreatedAt,
		ExpiresAt:     &job.ExpiresAt,
	}
	if job.HasArtifact() && !job.Expired(now) {
		v.DownloadURL = routes.APIAccountExport + "/download"
	}
	return v
}

func accountExportDownload(w http.ResponseWriter, r *http.Request, store accountexport.Store, objStore objects.Store, accountID string, log *slog.Logger) {
	if objStore == nil {
		http.Error(w, `{"error":"artifact store unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	job, err := store.LatestForAccount(r.Context(), accountID)
	if err != nil {
		log.Error("account export download lookup failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if job == nil || !job.HasArtifact() || job.Expired(time.Now()) {
		http.Error(w, `{"error":"no export available — request one first via GET /v1/api/account/export"}`, http.StatusNotFound)
		return
	}
	body, err := objStore.Get(r.Context(), job.ArtifactBucket, job.ArtifactKey)
	if err != nil {
		log.Error("account export artifact fetch failed", "key", job.ArtifactKey, "error", err)
		http.Error(w, `{"error":"artifact unavailable"}`, http.StatusBadGateway)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="account-export.zip"`)
	if _, err := io.Copy(w, body); err != nil {
		log.Error("account export stream failed", "error", err)
	}
}

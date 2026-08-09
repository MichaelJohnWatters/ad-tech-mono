package main

// products.go — the advertiser product catalog API (Dynamic Product Ads slice
// 1). A product feed is a second feed TYPE on the unified ingestion path (ADR
// 0007): same staging, same audience_ingest_jobs queue (kind=product), same
// inline-vs-202 threshold, same strict all-or-nothing validation — but the rows
// land in the products table, not segment memberships.
//
//	GET  /v1/api/products — list the account's catalog
//	POST /v1/api/products — upload a feed CSV (multipart: name + file)
//
// RBAC rides the audience-data domain (audiences:read / audiences:upload) —
// the catalog is ingested first-party data, same trust boundary as a CRM list.

import (
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/catalog"
	catalogpg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/catalog/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingest"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// productUploadResponse is the INLINE (small-feed) 200 response.
type productUploadResponse struct {
	ProductsWritten int `json:"products_written"`
	TotalRows       int `json:"total_rows"`
	ValidRows       int `json:"valid_rows"`
	RejectedRows    int `json:"rejected_rows"`
}

// productListResponse is the GET body: the account's catalog.
type productListResponse struct {
	Products []catalog.Product `json:"products"`
	Count    int               `json:"count"`
}

// productsHandler serves the tenant-scoped product catalog API. It reuses the
// audience upload deps (processor, queue, staging bucket, thresholds, email)
// plus the catalog store for reads.
func productsHandler(deps audienceDeps, cat *catalogpg.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, productListResponse{Products: []catalog.Product{}}) {
			return
		}
		if cat == nil || deps.proc == nil || deps.ingestStore == nil || deps.objects == nil {
			http.Error(w, `{"error":"catalog store unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "audiences:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			products, err := cat.ListByAccount(r.Context(), claims.AccountID, 500)
			if err != nil {
				deps.log.Error("product list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(productListResponse{Products: products, Count: len(products)})

		case http.MethodPost:
			if !can(claims, "audiences:upload") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			deps.handleProductUpload(w, r, claims)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// handleProductUpload mirrors handleUpload for kind=product: pre-flight →
// stage → enqueue → inline-or-202. Multipart only (feeds are files).
func (deps audienceDeps) handleProductUpload(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	accountID := claims.AccountID
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "multipart/form-data" {
		http.Error(w, `{"error":"product feeds are multipart/form-data uploads (fields: name, file)"}`, http.StatusBadRequest)
		return
	}
	req, raw, errMsg := parseMultipartUpload(w, r, deps.maxBytes)
	if errMsg != "" {
		http.Error(w, `{"error":`+jsonStr(errMsg)+`}`, http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}
	rowCount := csvDataRows(raw)
	if rowCount == 0 {
		http.Error(w, `{"error":"no product rows in upload"}`, http.StatusBadRequest)
		return
	}

	// Completion-email recipients, same policy as audience uploads.
	recipients := deps.resolveUploaderEmail(r.Context(), claims.UserID)
	recipients = append(recipients, req.AdditionalEmails...)
	notifyEmails := ingest.SanitizeNotifyEmails(recipients)
	spec := ingestjobs.SegmentSpec{Name: req.Name}

	// PRE-FLIGHT: 422 a wrong-shaped feed before staging.
	if err := deps.proc.ValidateProductSample(r.Context(), safeFileName(req.Name)+".csv", raw, 10); err != nil {
		deps.log.Info("product upload rejected (pre-flight)", "name", req.Name, "reason", err.Error())
		ingest.NotifyResult(context.Background(), deps.emailSender, deps.emailFrom,
			ingestjobs.Job{Kind: ingestjobs.KindProduct, SegmentSpec: spec, NotifyEmails: notifyEmails},
			ingestjobs.IngestResult{}, err, deps.log)
		http.Error(w, `{"error":`+jsonStr("file rejected: "+err.Error())+`}`, http.StatusUnprocessableEntity)
		return
	}

	fileKey := "api/" + accountID + "/" + uuid.NewString() + "/" + safeFileName(req.Name)
	if err := deps.objects.Put(r.Context(), deps.bucket, fileKey,
		bytes.NewReader(raw), int64(len(raw)), "text/csv"); err != nil {
		deps.log.Error("product upload: stage file failed", "name", req.Name, "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	runAt := now
	if s := strings.TrimSpace(req.RunAt); s != "" {
		t, perr := time.Parse(time.RFC3339, s)
		if perr != nil {
			http.Error(w, `{"error":"run_at must be RFC3339"}`, http.StatusBadRequest)
			return
		}
		if t.After(now) {
			runAt = t.UTC()
		}
	}

	job := ingestjobs.Job{
		AccountID:    accountID,
		Source:       ingestjobs.SourceAPI,
		Kind:         ingestjobs.KindProduct,
		TraceID:      tracing.TraceIDFromContext(r.Context()),
		FileBucket:   deps.bucket,
		FileKey:      fileKey,
		SegmentSpec:  spec,
		RunAt:        runAt,
		NotifyEmails: notifyEmails,
	}
	jobID, err := deps.ingestStore.Enqueue(r.Context(), job)
	if err != nil {
		deps.log.Error("product upload: enqueue failed", "name", req.Name, "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if jobID == "" {
		http.Error(w, `{"error":"upload already in progress"}`, http.StatusConflict)
		return
	}

	if rowCount <= deps.inlineMaxRow && !runAt.After(now) {
		deps.runProductInline(w, r, jobID, req.Name)
		return
	}
	held := ""
	if runAt.After(now) {
		held = runAt.Format(time.RFC3339)
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(audienceEnqueuedResponse{JobID: jobID, Status: ingestjobs.StatusQueued, RunAt: held})
	deps.log.Info("product upload queued", "trace_id", ingestjobs.TraceForID(jobID), "job", jobID,
		"name", req.Name, "rows", rowCount, "run_at", runAt.Format(time.RFC3339))
}

// runProductInline is runInline's kind=product twin: claim the enqueued row,
// run ProcessProducts, MarkDone/MarkFailed, map the result.
func (deps audienceDeps) runProductInline(w http.ResponseWriter, r *http.Request, jobID, name string) {
	log := deps.log.With("trace_id", ingestjobs.TraceForID(jobID), "job", jobID)
	claimed, err := deps.ingestStore.ClaimByID(r.Context(), jobID)
	if err != nil {
		log.Error("product upload: claim failed", "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if claimed == nil {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(audienceEnqueuedResponse{JobID: jobID, Status: ingestjobs.StatusRunning})
		return
	}

	result, err := deps.proc.ProcessProducts(r.Context(), *claimed)
	if err != nil {
		if ingest.IsInfra(err) && claimed.Attempts < claimed.MaxAttempts {
			log.Warn("product upload: inline run left for worker retry", "error", err)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(audienceEnqueuedResponse{JobID: jobID, Status: ingestjobs.StatusRunning})
			return
		}
		if markErr := deps.ingestStore.MarkFailed(r.Context(), jobID, err.Error()); markErr != nil {
			log.Error("product upload: mark failed", "error", markErr)
		}
		ingest.NotifyResult(context.Background(), deps.emailSender, deps.emailFrom, *claimed, result, err, deps.log)
		if ingest.IsReject(err) {
			log.Info("product upload rejected", "name", name, "reason", err.Error())
			http.Error(w, `{"error":`+jsonStr("file rejected: "+err.Error())+`}`, http.StatusUnprocessableEntity)
			return
		}
		log.Error("product upload failed", "name", name, "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if err := deps.ingestStore.MarkDone(r.Context(), jobID, result); err != nil {
		log.Error("product upload: mark done", "error", err)
	}
	ingest.NotifyResult(context.Background(), deps.emailSender, deps.emailFrom, *claimed, result, nil, deps.log)

	resp := productUploadResponse{
		ProductsWritten: result.MembersAdded,
		TotalRows:       result.TotalRows,
		ValidRows:       result.ValidRows,
		RejectedRows:    result.RejectedRows,
	}
	log.Info("product upload", "name", name, "written", resp.ProductsWritten,
		"valid", resp.ValidRows, "rejected", resp.RejectedRows)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

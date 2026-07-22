package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingest"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
)

// audienceUploadRequest is a CRM/audience upload: create-or-find a named
// segment for an account and bulk-add user memberships. Since ADR 0007 every
// upload — small or large — is staged to object storage and recorded as one
// audience_ingest_jobs row. Small, due-now files are processed INLINE (the same
// pkg/ingest.Processor the pipeline worker runs) so the response still carries a
// match rate; larger files are drained asynchronously by the pipeline ingest
// worker and the response is a job id to poll.
//
// account_id is IGNORED — the handler binds the segment to the authenticated
// account from the JWT claims, so a caller can never write another tenant's
// data. The field remains for backward-compatible request bodies.
type audienceUploadRequest struct {
	AccountID  string   `json:"account_id"`
	Name       string   `json:"name"`
	Type       string   `json:"type,omitempty"`       // default first_party
	Visibility string   `json:"visibility,omitempty"` // public | dsp_private (default dsp_private)
	Source     string   `json:"source,omitempty"`     // default crm_upload
	UserIDs    []string `json:"user_ids"`
}

// audienceUploadResponse is the INLINE (small-file) 200 response — a genuine
// terminal ingest with a match rate, mapped from the shared processor's
// IngestResult.
type audienceUploadResponse struct {
	SegmentID    string  `json:"segment_id"`
	MembersAdded int     `json:"members_added"`
	MembersSent  int     `json:"members_sent"`
	Matched      int     `json:"matched"`
	MatchRate    float64 `json:"match_rate"`
}

// audienceEnqueuedResponse is the ASYNC (large-file) 202 response — the file is
// staged + queued; the client polls GET /v1/api/audiences/ingest/{id}.
type audienceEnqueuedResponse struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

// audienceIngestStatusResponse is the job-status view (GET .../ingest/{id}).
type audienceIngestStatusResponse struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	SegmentID    string  `json:"segment_id,omitempty"`
	TotalRows    int     `json:"total_rows"`
	ValidRows    int     `json:"valid_rows"`
	RejectedRows int     `json:"rejected_rows"`
	MatchedRows  int     `json:"matched_rows"`
	MatchRate    float64 `json:"match_rate"`
	Error        string  `json:"error,omitempty"`
}

// audienceDeps bundles the audience-upload handler's collaborators: the segment
// store (list), the shared processor (inline run), the ingest queue (stage +
// enqueue + claim + status), the staging object store + bucket, and the
// inline/oversize thresholds.
type audienceDeps struct {
	store        *audiencepg.Store
	proc         *ingest.Processor
	ingestStore  ingestjobs.Store
	objects      objects.Store
	bucket       string
	inlineMaxRow int
	maxBytes     int
	log          *slog.Logger
}

// audienceHandler serves the tenant-scoped audiences API for the customer
// portal:
//
//	GET  /v1/api/audiences — list the account's segments (with member counts + match rate)
//	POST /v1/api/audiences — stage + enqueue a named segment upload
//	  - application/json: {name, type, visibility, user_ids}
//	  - multipart/form-data: fields name/type/visibility + a CSV file.
//	    PII must be hashed client-side before transmission (the portal does).
//
// Both bind to the authenticated account from the JWT claims; a body account_id
// is ignored. Small, due-now uploads run inline (200 + match rate); larger ones
// return 202 + a job id to poll.
func audienceHandler(deps audienceDeps) http.HandlerFunc {
	store, log := deps.store, deps.log
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, []audiencepg.Segment{}) {
			return
		}
		if store == nil || deps.proc == nil || deps.ingestStore == nil || deps.objects == nil {
			http.Error(w, `{"error":"audience store unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "audiences:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			segs, err := store.ListSegments(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("audience list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(segs)

		case http.MethodPost:
			if !can(claims, "audiences:upload") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			deps.handleUpload(w, r, claims.AccountID)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// handleUpload validates the upload synchronously, stages the raw bytes, and
// enqueues an ingest job. Small + due-now → claim + inline Process → 200 with a
// match rate; otherwise → leave queued → 202 with the job id.
func (deps audienceDeps) handleUpload(w http.ResponseWriter, r *http.Request, accountID string) {
	var req audienceUploadRequest
	var raw []byte // the CSV bytes staged to object storage
	var src string

	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "multipart/form-data" {
		var errMsg string
		req, raw, errMsg = parseMultipartUpload(w, r, deps.maxBytes)
		if errMsg != "" {
			http.Error(w, `{"error":`+jsonStr(errMsg)+`}`, http.StatusBadRequest)
			return
		}
		src = "portal_csv"
	} else {
		if err := json.NewDecoder(io.LimitReader(r.Body, int64(deps.maxBytes)+1)).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		raw = userIDsToCSV(req.UserIDs)
		src = "crm_upload"
	}

	// Bind to the authenticated tenant — never trust a body account_id.
	req.AccountID = accountID
	if req.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}
	if len(raw) > deps.maxBytes {
		http.Error(w, fmt.Sprintf(`{"error":"upload exceeds %d bytes"}`, deps.maxBytes), http.StatusBadRequest)
		return
	}
	rowCount := csvDataRows(raw)
	if rowCount == 0 {
		http.Error(w, `{"error":"no user ids in upload"}`, http.StatusBadRequest)
		return
	}
	if req.Type == "" {
		req.Type = "first_party"
	}
	if !isValidSegmentType(req.Type) {
		http.Error(w, `{"error":"invalid type"}`, http.StatusBadRequest)
		return
	}
	if req.Visibility == "" {
		req.Visibility = "dsp_private"
	}
	if req.Visibility != "public" && req.Visibility != "dsp_private" {
		http.Error(w, `{"error":"visibility must be public or dsp_private"}`, http.StatusBadRequest)
		return
	}
	if req.Source == "" {
		req.Source = src
	}

	// Stage the raw bytes so a crashed inline run is recoverable by the worker,
	// and so every upload is durable + recorded identically (ADR 0007).
	fileKey := "api/" + accountID + "/" + uuid.NewString() + "/" + safeFileName(req.Name)
	if err := deps.objects.Put(r.Context(), deps.bucket, fileKey,
		bytes.NewReader(raw), int64(len(raw)), "text/csv"); err != nil {
		deps.log.Error("audience upload: stage file failed", "name", req.Name, "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	spec := ingestjobs.SegmentSpec{
		Name:       req.Name,
		Type:       req.Type,
		Visibility: req.Visibility,
		Consent:    true, // uploader-declared basis: first-party CRM data
		IDType:     "user_id",
		Access:     req.Source,
	}
	job := ingestjobs.Job{
		AccountID:   accountID,
		Source:      ingestjobs.SourceAPI,
		FileBucket:  deps.bucket,
		FileKey:     fileKey,
		SegmentSpec: spec,
		RunAt:       time.Now().UTC(),
	}
	jobID, err := deps.ingestStore.Enqueue(r.Context(), job)
	if err != nil {
		deps.log.Error("audience upload: enqueue failed", "name", req.Name, "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if jobID == "" {
		// Dedupe hit (a job for the same staged key is already in flight) — the
		// uuid'd key makes this practically impossible, but stay honest.
		http.Error(w, `{"error":"upload already in progress"}`, http.StatusConflict)
		return
	}

	// Small + due-now → run inline for an instant match rate. Larger → leave it
	// queued for the pipeline ingest worker.
	if rowCount <= deps.inlineMaxRow {
		deps.runInline(w, r, jobID, req)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(audienceEnqueuedResponse{JobID: jobID, Status: ingestjobs.StatusQueued})
	deps.log.Info("audience upload queued", "job", jobID, "name", req.Name, "rows", rowCount)
}

// runInline claims the row it just enqueued (taking the worker's lease) and
// runs the shared processor. Success → MarkDone + 200 with the match rate; an
// inline Process error → MarkFailed + 500. If the row can't be claimed (a peer
// worker beat us to it) the job is already being processed — return 202.
func (deps audienceDeps) runInline(w http.ResponseWriter, r *http.Request, jobID string, req audienceUploadRequest) {
	claimed, err := deps.ingestStore.ClaimByID(r.Context(), jobID)
	if err != nil {
		deps.log.Error("audience upload: claim failed", "job", jobID, "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if claimed == nil {
		// A worker already claimed it — hand back the job id to poll.
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(audienceEnqueuedResponse{JobID: jobID, Status: ingestjobs.StatusRunning})
		return
	}

	result, err := deps.proc.Process(r.Context(), *claimed)
	if err != nil {
		// Infra failure mid-inline: leave the lease to lapse so the worker
		// reprocesses (idempotent AddMembers), UNLESS attempts are exhausted.
		if ingest.IsInfra(err) && claimed.Attempts < claimed.MaxAttempts {
			deps.log.Warn("audience upload: inline run left for worker retry", "job", jobID, "error", err)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(audienceEnqueuedResponse{JobID: jobID, Status: ingestjobs.StatusRunning})
			return
		}
		if markErr := deps.ingestStore.MarkFailed(r.Context(), jobID, err.Error()); markErr != nil {
			deps.log.Error("audience upload: mark failed", "job", jobID, "error", markErr)
		}
		deps.log.Error("audience upload failed", "name", req.Name, "job", jobID, "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if err := deps.ingestStore.MarkDone(r.Context(), jobID, result); err != nil {
		deps.log.Error("audience upload: mark done", "job", jobID, "error", err)
	}

	resp := audienceUploadResponse{
		SegmentID:    result.SegmentID,
		MembersAdded: result.MembersAdded,
		MembersSent:  result.ValidRows,
		Matched:      result.MatchedRows,
		MatchRate:    result.MatchRate,
	}
	deps.log.Info("audience upload", "segment", resp.SegmentID, "name", req.Name, "job", jobID,
		"added", resp.MembersAdded, "sent", resp.MembersSent,
		"matched", resp.Matched, "match_rate", resp.MatchRate)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// audienceIngestStatusHandler serves GET /v1/api/audiences/ingest/{id} —
// tenant-scoped job status (queued/running/done/failed) with the terminal
// counts + match rate.
func audienceIngestStatusHandler(ingestStore ingestjobs.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !can(claims, "audiences:read") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if ingestStore == nil {
			http.Error(w, `{"error":"audience store unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, routes.APIAudienceIngest)
		id = strings.Trim(id, "/")
		if id == "" || strings.Contains(id, "/") {
			http.Error(w, `{"error":"job id required"}`, http.StatusBadRequest)
			return
		}
		job, err := ingestStore.GetByAccount(r.Context(), claims.AccountID, id)
		if err != nil {
			log.Error("audience ingest status failed", "job", id, "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if job == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(audienceIngestStatusResponse{
			ID: job.ID, Status: job.Status, SegmentID: job.SegmentID,
			TotalRows: job.TotalRows, ValidRows: job.ValidRows, RejectedRows: job.RejectedRows,
			MatchedRows: job.MatchedRows, MatchRate: job.MatchRate, Error: job.Error,
		})
	}
}

// userIDsToCSV renders JSON user_ids into the same CSV shape the drop-zone
// stages, so both variants flow through the one processor. Non-empty ids only.
func userIDsToCSV(ids []string) []byte {
	var buf bytes.Buffer
	buf.WriteString("user_id\n")
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			buf.WriteString(id)
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}

// csvDataRows cheaply counts non-empty data lines (excluding the header) — the
// inline/enqueue predicate. Cheap by design: it never fully parses the file.
func csvDataRows(body []byte) int {
	n := 0
	for i, line := range bytes.Split(body, []byte{'\n'}) {
		if i == 0 { // header
			continue
		}
		if len(bytes.TrimSpace(line)) > 0 {
			n++
		}
	}
	return n
}

// safeFileName sanitises a segment name into an object-key-safe file name.
func safeFileName(name string) string {
	name = strings.TrimSpace(name)
	repl := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}
	name = strings.Map(repl, name)
	if name == "" {
		name = "upload"
	}
	return name + ".csv"
}

// parseMultipartUpload handles the CSV file variant. Returns the metadata
// fields, the raw CSV bytes to stage, and a non-empty error message on caller
// error. The portal path is delimiter-separated text only (the browser hashes
// PII cell-by-cell before upload, which it can't do inside binary formats);
// compressed / parquet uploads belong in the partner drop-zone, which decodes
// them server-side.
func parseMultipartUpload(w http.ResponseWriter, r *http.Request, maxBytes int) (audienceUploadRequest, []byte, string) {
	var req audienceUploadRequest
	r.Body = http.MaxBytesReader(w, r.Body, int64(maxBytes))
	if err := r.ParseMultipartForm(int64(maxBytes)); err != nil {
		return req, nil, "file too large or malformed multipart body"
	}
	req.Name = strings.TrimSpace(r.FormValue("name"))
	req.Type = r.FormValue("type")
	req.Visibility = r.FormValue("visibility")
	file, _, err := r.FormFile("file")
	if err != nil {
		return req, nil, "missing file field"
	}
	defer file.Close()
	body, err := io.ReadAll(file)
	if err != nil {
		return req, nil, "read file: " + err.Error()
	}
	switch {
	case bytes.HasPrefix(body, []byte{0x50, 0x4b, 0x03, 0x04}),
		bytes.HasPrefix(body, []byte{0x1f, 0x8b}):
		return req, nil, "compressed uploads are not supported here — use the partner drop-zone"
	case bytes.HasPrefix(body, []byte("PAR1")):
		return req, nil, "parquet uploads are not supported here — use the partner drop-zone"
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return req, nil, "empty file"
	}
	return req, body, ""
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func isValidSegmentType(t string) bool {
	switch t {
	case "first_party", "behavioral", "lookalike", "suppression",
		"retargeting", "composite", "predictive", "cdp_imported":
		return true
	}
	return false
}

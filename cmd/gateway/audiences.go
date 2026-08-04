package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audiencemappings"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/dataproviders"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/email"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingest"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pgp"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
)

// audiencePGPKeyResponse is the GET /v1/api/audiences/pgp-key body (ADR 0008):
// the platform PUBLIC key (armored) + its fingerprint, derived on demand from
// the pgp_private secrets so the public half is never stored separately. The
// top-level pair is the ACTIVE key (encrypt to this); `keys` is the full overlap
// set — during a rotation grace window the active AND rotating keys both decrypt
// on ingest, so a provider that encrypted to either is still accepted (Phase I).
type audiencePGPKeyResponse struct {
	PublicKey   string            `json:"public_key"`
	Fingerprint string            `json:"fingerprint"`
	Keys        []audiencePGPItem `json:"keys"`
}

// audiencePGPItem is one entry in the PGP keyset. `status` tells a provider which
// key to prefer (active) versus which is still accepted while it winds down
// (rotating).
type audiencePGPItem struct {
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	Status      string `json:"status"`
}

// audiencePGPKeyHandler serves the platform PGP public key for provider-side
// encryption. Reads the active pgp_private secret from the warm cache and
// derives the public key + fingerprint. 503 if the cache is nil; 404 if no key
// is configured. JWT-gated (the key is public, but the endpoint sits behind the
// app so only authenticated tenant users see it).
func audiencePGPKeyHandler(cache *secrets.Cache, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if middleware.ClaimsFromContext(r.Context()) == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if cache == nil {
			http.Error(w, `{"error":"secrets unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		active, ok := cache.LookupActiveByPurpose(secrets.PurposePGPPrivate)
		if !ok || active.Value == "" {
			http.Error(w, `{"error":"no PGP key configured"}`, http.StatusNotFound)
			return
		}
		pub, fp, err := pgp.PublicArmorFromPrivate(active.Value)
		if err != nil {
			log.Error("audience pgp-key: derive public failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		// Build the overlap keyset: every non-revoked key's public half, active
		// first. A rotating key that fails to derive is skipped, not fatal.
		resp := audiencePGPKeyResponse{PublicKey: pub, Fingerprint: fp}
		for _, s := range cache.NonRevokedByPurpose(secrets.PurposePGPPrivate) {
			if s.Value == "" {
				continue
			}
			kpub, kfp, kerr := pgp.PublicArmorFromPrivate(s.Value)
			if kerr != nil {
				log.Warn("audience pgp-key: skip keyset entry (derive failed)", "status", s.Status, "error", kerr)
				continue
			}
			resp.Keys = append(resp.Keys, audiencePGPItem{PublicKey: kpub, Fingerprint: kfp, Status: s.Status})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

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
	// RunAt (RFC3339, optional) holds the file until this time before it is
	// processed — for data-licensing / embargo / point-in-time freshness (ADR
	// 0007). Empty or past = process now. A future run_at forces the async
	// (202) path: the file is staged but never opened until due. NOTE this is
	// INGEST timing, not go-live — when the audience is served is still
	// controlled by campaign/line-item flight dates.
	RunAt string `json:"run_at,omitempty"`
	// MappingID (optional, ADR 0008) applies a saved tenant-scoped custom field
	// mapping ("connector") to this upload: the handler loads the mapping and
	// sets SegmentSpec.FieldMappings + IDType from it. Empty = default behaviour
	// (the built-in id column names). Multipart form field: mapping_id.
	MappingID string `json:"mapping_id,omitempty"`
	// ProviderID (optional, ADR 0009) attributes this upload to a saved data
	// provider: the handler snapshots the provider's data-party, licence,
	// default id_type, and notify recipients onto the job. Empty = no provider
	// (plain first-party upload). Multipart form field: provider_id.
	ProviderID string `json:"provider_id,omitempty"`
	// AdditionalEmails (optional, ADR 0008 Feature 3) are extra recipients
	// notified when this upload's ingest job finishes — in addition to the
	// uploader, who is always notified (resolved from team_members by the JWT
	// UserID). Multipart form field: additional_emails (comma/space separated).
	AdditionalEmails []string `json:"additional_emails,omitempty"`
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
	// RunAt is set when the file is HELD (future run_at) — the time it will be
	// processed. Empty for a normal large-file queue (processed on the next tick).
	RunAt string `json:"run_at,omitempty"`
}

// audienceIngestListItem is one row of the account's upload history
// (GET .../ingest/ with no id) — the portal shows status + counts + reason.
type audienceIngestListItem struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Source       string  `json:"source"`
	Status       string  `json:"status"`
	ValidRows    int     `json:"valid_rows"`
	RejectedRows int     `json:"rejected_rows"`
	MatchRate    float64 `json:"match_rate"`
	// DataParty is the provider's party classification for this upload (ADR
	// 0009): first | second | third. Empty for a plain first-party upload.
	DataParty  string `json:"data_party,omitempty"`
	Error      string `json:"error,omitempty"`
	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at,omitempty"`
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
	// mappingStore loads a tenant's saved custom field mapping when an upload
	// carries mapping_id (ADR 0008). nil is tolerated — an upload with no
	// mapping_id behaves exactly as before.
	mappingStore audiencemappings.Store
	// providerStore loads a tenant's data provider when an upload carries
	// provider_id (ADR 0009); the upload snapshots the provider's party/licence/
	// id_type/notify defaults. nil is tolerated — no provider_id is unaffected.
	providerStore dataproviders.Store
	// db resolves the uploader's email from team_members by the JWT UserID for
	// ingest completion emails (ADR 0008 Feature 3). nil is tolerated — the
	// upload falls back to the additional_emails list only.
	db *sql.DB
	// emailSender + emailFrom deliver the inline ingest completion email. A nil
	// sender (or an empty recipient list) is a no-op — email is best-effort.
	emailSender email.Sender
	emailFrom   string
	log         *slog.Logger
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
			deps.handleUpload(w, r, claims)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// handleUpload validates the upload synchronously, stages the raw bytes, and
// enqueues an ingest job. Small + due-now → claim + inline Process → 200 with a
// match rate; otherwise → leave queued → 202 with the job id.
func (deps audienceDeps) handleUpload(w http.ResponseWriter, r *http.Request, claims *auth.Claims) {
	accountID := claims.AccountID
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

	spec := ingestjobs.SegmentSpec{
		Name:       req.Name,
		Type:       req.Type,
		Visibility: req.Visibility,
		Consent:    true, // uploader-declared basis: first-party CRM data
		IDType:     "user_id",
		Access:     req.Source,
	}

	// ADR 0009: attribute the upload to a data provider if one was selected, and
	// snapshot its defaults onto the job (self-describing, per ADR 0007). Tenant-
	// scoped — GetByAccount 404s a provider that isn't the caller's. Party/licence/
	// id_type come from the provider; the mapping block below may still override
	// id_type (a mapping is built for this specific file, so it's more specific).
	// Provider notify recipients are appended to the completion-email list.
	providerID := ""
	var providerNotify []string
	if pid := strings.TrimSpace(req.ProviderID); pid != "" {
		if deps.providerStore == nil {
			http.Error(w, `{"error":"providers unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		provider, err := deps.providerStore.GetByAccount(r.Context(), accountID, pid)
		if err != nil {
			deps.log.Error("audience upload: load provider failed", "provider", pid, "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if provider == nil {
			http.Error(w, `{"error":"provider not found"}`, http.StatusNotFound)
			return
		}
		providerID = provider.ID
		spec.DataParty = provider.DefaultParty
		spec.Access = provider.DefaultLicence
		spec.EncryptionExpected = provider.EncryptionExpected
		if provider.DefaultIDType != "" {
			spec.IDType = provider.DefaultIDType
		}
		providerNotify = provider.NotifyEmails
	}

	// ADR 0008: apply a saved custom field mapping ("connector") if the upload
	// references one. Tenant-scoped — GetByAccount 404s a mapping that isn't the
	// caller's, so a tenant can never apply another tenant's connector. The
	// mapping sets FieldMappings (their column → our canonical field) + IDType;
	// the validate + process below run with it applied. No mapping_id → unchanged.
	if mid := strings.TrimSpace(req.MappingID); mid != "" {
		if deps.mappingStore == nil {
			http.Error(w, `{"error":"mappings unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		mapping, err := deps.mappingStore.GetByAccount(r.Context(), accountID, mid)
		if err != nil {
			deps.log.Error("audience upload: load mapping failed", "mapping", mid, "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
		if mapping == nil {
			http.Error(w, `{"error":"mapping not found"}`, http.StatusNotFound)
			return
		}
		spec.FieldMappings = mapping.Mappings
		if mapping.IDType != "" {
			spec.IDType = mapping.IDType
		}
	}

	// Completion-email recipients (ADR 0008 Feature 3): the uploader (resolved
	// from team_members by the JWT UserID) is always notified; additional_emails
	// are appended. A failed uploader lookup still notifies the additional list.
	// Computed BEFORE the pre-flight so a rejected file still emails a failure.
	recipients := deps.resolveUploaderEmail(r.Context(), claims.UserID)
	recipients = append(recipients, req.AdditionalEmails...)
	recipients = append(recipients, providerNotify...) // provider default recipients (ADR 0009)
	notifyEmails := ingest.SanitizeNotifyEmails(recipients)

	// PRE-FLIGHT: sample-parse the first rows (lowercased headers) and reject a
	// wrong-shaped file with a 422 BEFORE staging — instant feedback, and a bad
	// file never enters the queue. Full per-row validation still runs at process
	// time (partial files import the good rows + quarantine the bad).
	if err := deps.proc.ValidateSample(r.Context(), safeFileName(req.Name)+".csv", raw, spec, 10); err != nil {
		deps.log.Info("audience upload rejected (pre-flight)", "name", req.Name, "reason", err.Error())
		// Notify recipients of the failure even though no job was created — the
		// pre-flight reject IS the terminal outcome. Synthetic job carries the
		// segment name + recipients for the email body.
		ingest.NotifyResult(context.Background(), deps.emailSender, deps.emailFrom,
			ingestjobs.Job{SegmentSpec: spec, NotifyEmails: notifyEmails}, ingestjobs.IngestResult{}, err, deps.log)
		http.Error(w, `{"error":`+jsonStr("file rejected: "+err.Error())+`}`, http.StatusUnprocessableEntity)
		return
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
	// run_at: hold the file until this time (licensing / embargo / freshness).
	// Empty or in the past → now. A future run_at forces the async path below.
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
		ProviderID:   providerID,
		FileBucket:   deps.bucket,
		FileKey:      fileKey,
		SegmentSpec:  spec,
		RunAt:        runAt,
		NotifyEmails: notifyEmails,
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

	// Small AND due-now → run inline for an instant match rate. Larger OR held
	// (future run_at) → leave it queued for the pipeline ingest worker; a held
	// file is staged but never opened until run_at (ClaimOne gates on it).
	if rowCount <= deps.inlineMaxRow && !runAt.After(now) {
		deps.runInline(w, r, jobID, req)
		return
	}
	held := ""
	if runAt.After(now) {
		held = runAt.Format(time.RFC3339)
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(audienceEnqueuedResponse{JobID: jobID, Status: ingestjobs.StatusQueued, RunAt: held})
	deps.log.Info("audience upload queued", "job", jobID, "name", req.Name, "rows", rowCount,
		"run_at", runAt.Format(time.RFC3339))
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
		// Best-effort completion email (ADR 0008). Background ctx so it isn't
		// cancelled when the HTTP response returns; a send failure never fails
		// the request.
		ingest.NotifyResult(context.Background(), deps.emailSender, deps.emailFrom, *claimed, result, err, deps.log)
		// A content REJECT (bad file) is a 422 client error, not a 500 — though
		// the pre-flight ValidateSample catches nearly all of these before here.
		if ingest.IsReject(err) {
			deps.log.Info("audience upload rejected", "name", req.Name, "job", jobID, "reason", err.Error())
			http.Error(w, `{"error":`+jsonStr("file rejected: "+err.Error())+`}`, http.StatusUnprocessableEntity)
			return
		}
		deps.log.Error("audience upload failed", "name", req.Name, "job", jobID, "error", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if err := deps.ingestStore.MarkDone(r.Context(), jobID, result); err != nil {
		deps.log.Error("audience upload: mark done", "job", jobID, "error", err)
	}
	// Best-effort completion email (ADR 0008). Background ctx so it isn't
	// cancelled when the HTTP response returns.
	ingest.NotifyResult(context.Background(), deps.emailSender, deps.emailFrom, *claimed, result, nil, deps.log)

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
		if strings.Contains(id, "/") {
			http.Error(w, `{"error":"bad job id"}`, http.StatusBadRequest)
			return
		}
		// No id → LIST the account's recent ingest jobs (the portal's upload
		// history: success/failed/queued/running + reason). Tenant-scoped.
		if id == "" {
			jobs, err := ingestStore.ListByAccount(r.Context(), claims.AccountID, 50)
			if err != nil {
				log.Error("audience ingest list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			out := make([]audienceIngestListItem, 0, len(jobs))
			for _, j := range jobs {
				item := audienceIngestListItem{
					ID: j.ID, Name: j.SegmentSpec.Name, Source: j.Source, Status: j.Status,
					ValidRows: j.ValidRows, RejectedRows: j.RejectedRows, MatchRate: j.MatchRate,
					DataParty: j.SegmentSpec.DataParty,
					Error:     j.Error, CreatedAt: j.CreatedAt.Format(time.RFC3339),
				}
				if j.FinishedAt != nil {
					item.FinishedAt = j.FinishedAt.Format(time.RFC3339)
				}
				out = append(out, item)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": out})
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
	req.RunAt = strings.TrimSpace(r.FormValue("run_at"))
	req.MappingID = strings.TrimSpace(r.FormValue("mapping_id"))
	req.ProviderID = strings.TrimSpace(r.FormValue("provider_id"))
	req.AdditionalEmails = splitEmails(r.FormValue("additional_emails"))
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

// resolveUploaderEmail looks up the uploader's email from team_members by the
// JWT UserID (ADR 0008 Feature 3). Returns nil (no primary recipient) on any
// failure — a nil db, a non-UUID dev user, an absent row, or a query error —
// so the upload still notifies the additional_emails list. The lookup is not
// tenant-filtered because team_members has no RLS and the id is the JWT's own
// (a caller can't spoof another user's UserID).
func (deps audienceDeps) resolveUploaderEmail(ctx context.Context, userID string) []string {
	// The JWT UserID is "user-<team_members.id>", so recover the bare id before the
	// uuid cast — otherwise every lookup throws 22P02 and the uploader is silently
	// never notified.
	userID = auth.TeamMemberID(userID)
	if deps.db == nil || userID == "" {
		return nil
	}
	var e string
	if err := deps.db.QueryRowContext(ctx,
		`SELECT email FROM team_members WHERE id = $1::uuid`, userID).Scan(&e); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			deps.log.Warn("audience upload: uploader email lookup failed", "user", userID, "error", err)
		}
		return nil
	}
	if e = strings.TrimSpace(e); e == "" {
		return nil
	}
	return []string{e}
}

// splitEmails splits a comma/space-separated recipient string into trimmed,
// non-empty entries. Sanitisation (dedup + "@" check) is ingest.SanitizeNotifyEmails.
func splitEmails(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// connectIngestEmail selects SMTP (Mailpit/SES) when gateway.smtp_host is set,
// else an in-memory sender that only logs deliveries (dev). Mirrors
// cmd/report-runner's connectEmail.
func connectIngestEmail(cfg *config.Config, from string, log *slog.Logger) email.Sender {
	host := keys.Gateway.SMTPHost.Get(cfg)
	if host == "" {
		log.Info("gateway ingest email via memory sender (no smtp_host set) — deliveries are logged only")
		return email.NewMemory(log)
	}
	if user := keys.Gateway.SMTPUsername.Get(cfg); user != "" {
		log.Info("gateway ingest email via authenticated SMTP", "host", host, "username", user)
		return email.NewSMTPAuth(host, from, user, keys.Gateway.SMTPPassword.Get(cfg), log)
	}
	log.Info("gateway ingest email via SMTP", "host", host)
	return email.NewSMTP(host, from, log)
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

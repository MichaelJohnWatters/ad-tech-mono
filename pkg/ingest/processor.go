// Package ingest is the single, shared audience-file processor (ADR 0007).
//
// Both producers — the 3rd-party drop-zone poller in cmd/pipeline and the
// 1st-party gateway upload — stage a file to object storage and record one row
// in audience_ingest_jobs (pkg/ingestjobs). Processor.Process is the one code
// path that turns a staged file into segment memberships + a match rate +
// published profile.signal events. The pipeline ingest worker runs it
// asynchronously; the gateway runs it inline for small, due-now uploads. There
// is no second matching path.
package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pgp"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// pgpRejectReason is the content-rejection message for a file that is
// PGP-encrypted but can't be decrypted (no key configured, wrong key, or
// corrupt). Shared by Process and ValidateSample so both paths reject
// identically (422 sync / failed job async).
const pgpRejectReason = "file is PGP-encrypted but could not be decrypted (wrong key or no key configured)"

// encryptionRequiredReason is the content-rejection for a cleartext file from a
// provider whose contract requires PGP encryption (ADR 0009 encryption_expected).
const encryptionRequiredReason = "this data provider requires PGP-encrypted files, but the uploaded file is not encrypted — encrypt it to the platform public key and retry"

// signalChunk caps how many ids ride in one ProfileSignalEvent so a huge staged
// file fans out to bounded NATS messages rather than one giant payload.
const signalChunk = 1000

// IdentityCounter is the match-rate seam (implemented by
// pkg/store/postgres.Store): "how many of these ids does the identity graph
// know?"
type IdentityCounter interface {
	CountKnownIdentifiers(ctx context.Context, ids []string) (int, error)
}

// Processor turns a staged audience file into segment memberships, a match
// rate, and published profile.signal events. It is dependency-injected so both
// cmd/pipeline (worker) and cmd/gateway (inline) build one from their own
// connections.
type Processor struct {
	Objects  objects.Store     // staged files live here (read + quarantine + move)
	Audience *audiencepg.Store // segment upsert + membership writes
	Matcher  IdentityCounter   // match-rate numerator (nil-safe)
	Pipeline *pipeline.Pipeline
	Bus      events.EventBus // profile.signal + cache-invalidate (nil-safe)
	Log      *slog.Logger
	// PGPKeyring holds the platform private key(s) for decrypt-on-ingest (ADR
	// 0008). nil/empty = no key configured: PGP files can't be decrypted and are
	// rejected as content failures; plaintext files are unaffected.
	PGPKeyring openpgp.EntityList
	// MaxRejectPct is the max % of a file's rows that may quarantine before the
	// WHOLE file is rejected (atomic per file — import none of it). 0 = strict
	// all-or-nothing (any bad row rejects); 100 = never reject on bad rows
	// (partial import, quarantine the rest). Default 0.
	MaxRejectPct int
}

// defaultIDMappings normalize the common id column names to id_value so a file
// that already uses one of these needs no explicit mapping.
var defaultIDMappings = map[string]string{
	"user_id": "id_value", "id": "id_value", "hashed_email": "id_value",
	"email_sha256": "id_value", "uid2": "id_value", "device_id": "id_value",
}

// idTypeForColumn maps a mapped-away source column to the id_type it implies,
// when the spec doesn't say and the row has no id_type column.
var idTypeForColumn = map[string]string{
	"hashed_email": "hashed_email", "email_sha256": "hashed_email",
	"uid2": "uid2", "device_id": "device_id",
}

// infraErr marks a retryable INFRA failure — the caller (ingest worker or
// gateway inline path) leaves the job for a lease-lapse retry and records
// nothing, because the run never happened. A PARTIAL content outcome (some
// valid rows, some quarantined) returns (result, nil): the file is processed
// and the job is done.
type infraErr struct{ err error }

func (e infraErr) Error() string { return e.err.Error() }

// IsInfra reports whether err is a retryable infra failure (unwrapped via
// errors.As). Callers use it to decide retry-vs-fail.
func IsInfra(err error) bool {
	var i infraErr
	return errors.As(err, &i)
}

// rejectErr marks a terminal CONTENT rejection — the file could not be used at
// all (undecodable, no data rows, or NO row had a usable id). The job is marked
// FAILED with this reason (shown in the onboarding monitor), never retried; the
// synchronous upload path maps it to HTTP 422. Distinct from a PARTIAL outcome
// (some rows valid) which still succeeds and imports the good rows.
type rejectErr struct{ reason string }

func (e rejectErr) Error() string { return e.reason }

// Reject builds a terminal content-rejection error carrying a human reason.
func Reject(reason string) error { return rejectErr{reason} }

// IsReject reports whether err is a content rejection (vs infra / other).
func IsReject(err error) bool {
	var r rejectErr
	return errors.As(err, &r)
}

// Process is the single audience-ingest processor: it reads the staged file at
// job.FileBucket/job.FileKey, runs decode → field-map → validate/normalise →
// CreateSegment/AddMembers → match rate → publish profile.signal in chunks →
// quarantine rejects, and returns the terminal counts. It moves the source file
// out of incoming/; the caller records the run on the audience_ingest_jobs row
// via MarkDone (the terminal counts fold the old onboarding_runs table, ADR 0007
// Phase 4). An INFRA error is returned wrapped so the caller retries; content
// failures return a result with nil error.
// ingestJobTrace is the batch-lineage correlation for the rows an upload produces —
// deliberately NOT a trace_id and deliberately a DISTINCT format so the two are never
// confused. Uploaded data is batch: it has no single request trace spanning it (the
// async worker processes it later, under no request), so its lineage is the ingest
// JOB. We format that as "ing_<32hex>": audience_ingest_jobs.id is a UUID (128 bits),
// dash-stripped to 32 hex and prefixed with ing_ — the prefix makes it self-identifying
// (never a 32-hex request trace) while still mapping deterministically back to the job.
// A row's real request trace, when one exists (inline upload), rides trace_id separately.
func ingestJobTrace(job ingestjobs.Job) string {
	return "ing_" + strings.ReplaceAll(job.ID, "-", "")
}

func (p *Processor) Process(ctx context.Context, job ingestjobs.Job) (ingestjobs.IngestResult, error) {
	started := time.Now().UTC()
	provider, key := job.Provider, job.FileKey
	bucket := job.FileBucket
	spec := job.SegmentSpec
	accountID := job.AccountID
	log := p.Log.With("provider", provider, "file", key, "job", job.ID)

	body, err := p.readObject(ctx, bucket, key)
	if err != nil {
		log.Error("ingest: read file failed (will retry)", "error", err)
		return ingestjobs.IngestResult{}, infraErr{err}
	}
	// ADR 0008: decrypt before decode. A plaintext file passes through; a
	// PGP-encrypted file we can't decrypt is a content reject (same path as an
	// undecodable file — quarantine the whole file, fail the job).
	body, wasEncrypted, derr := pgp.MaybeDecrypt(body, p.PGPKeyring)
	if derr != nil {
		log.Error("ingest: pgp decrypt failed", "encrypted", wasEncrypted, "error", derr)
		return p.quarantineStaged(ctx, bucket, provider, key, accountID, started, pgpRejectReason)
	}
	// ADR 0009: enforce the provider's encryption contract. A cleartext file from
	// an encryption_expected provider is a content reject (defense for the async /
	// drop-zone paths; the gateway pre-flight rejects it up front too).
	if spec.EncryptionExpected && !wasEncrypted {
		log.Warn("ingest: cleartext file from encryption-required provider rejected")
		return p.quarantineStaged(ctx, bucket, provider, key, accountID, started, encryptionRequiredReason)
	}
	records, err := DecodeFile(ctx, path.Base(key), body)
	if err != nil {
		return p.quarantineStaged(ctx, bucket, provider, key, accountID, started, fmt.Sprintf("decode: %v", err))
	}
	if len(records) == 0 {
		return p.quarantineStaged(ctx, bucket, provider, key, accountID, started, "no data rows")
	}

	mappings, required := buildMappings(spec)
	// Which source column became id_value — for the implied id_type.
	sourceIDColumn := ""
	for col := range records[0] {
		if mappings[col] == "id_value" {
			sourceIDColumn = col
			break
		}
	}

	result := p.Pipeline.Process(ctx, records, pipeline.PublisherConfig{
		PublisherID:    provider,
		Format:         "csv",
		RequiredFields: required,
		FieldMappings:  mappings,
	})

	// Persist the quarantine FIRST — rejected rows must survive even if the
	// rest of the run fails and retries.
	rejectedKey := ""
	if len(result.Quarantine) > 0 {
		rejectedKey = rejectedPrefix(provider) + path.Base(key)
		if err := p.writeRejected(ctx, bucket, rejectedKey, result.Quarantine); err != nil {
			log.Error("ingest: persist quarantine failed (will retry)", "error", err)
			return ingestjobs.IngestResult{}, infraErr{err}
		}
	}
	if len(result.Valid) == 0 {
		// REJECT: the file parsed but no row had a usable id. Quarantine the
		// whole file (move to rejected/) and fail the job with a reason so the
		// monitor shows WHY — rather than silently importing zero members.
		if _, err := p.finishFile(ctx, bucket, provider, key, accountID, started, "", 0, result, rejectedKey,
			"no valid rows after validation"); err != nil {
			return ingestjobs.IngestResult{}, err // infra move failure → retry
		}
		return ingestjobs.IngestResult{RejectedRows: len(result.Quarantine), RejectedKey: rejectedKey},
			rejectErr{fmt.Sprintf("no usable id column: none of the %d rows had a non-empty id_value after field-mapping", result.Stats.TotalInput)}
	}

	// Atomic per-file: a file with bad rows imports NONE of it. The rejected
	// rows are persisted to rejected/ above so the operator sees exactly which
	// failed; the job fails with the count (shown in the monitor / 422 on the
	// sync path). Tunable via ingest.max_reject_pct — 0 = strict all-or-nothing
	// (any bad row rejects the whole file); e.g. 20 imports the good rows as
	// long as ≤20% quarantined (messy-partner-feed mode).
	total := len(result.Valid) + len(result.Quarantine)
	if len(result.Quarantine) > 0 && len(result.Quarantine)*100 > p.MaxRejectPct*total {
		if _, err := p.finishFile(ctx, bucket, provider, key, accountID, started, "", 0, result, rejectedKey,
			"rejected rows exceed threshold"); err != nil {
			return ingestjobs.IngestResult{}, err // infra move failure → retry
		}
		return ingestjobs.IngestResult{RejectedRows: len(result.Quarantine), RejectedKey: rejectedKey},
			rejectErr{fmt.Sprintf("%d of %d rows failed validation — file rejected, nothing imported%s (see %s)",
				len(result.Quarantine), total, firstQuarantineDetail(result.Quarantine), rejectedKey)}
	}

	ids := make([]events.ProfileSignalID, 0, len(result.Valid))
	values := make([]string, 0, len(result.Valid))
	impliedType := spec.IDType
	if impliedType == "" {
		impliedType = idTypeForColumn[sourceIDColumn]
	}
	if impliedType == "" {
		impliedType = "user_id"
	}
	for _, rec := range result.Valid {
		v := strings.TrimSpace(rec["id_value"])
		if v == "" {
			continue
		}
		t := strings.TrimSpace(rec["id_type"])
		if t == "" {
			t = impliedType
		}
		ids = append(ids, events.ProfileSignalID{IDType: t, IDValue: v})
		values = append(values, v)
	}

	segName := spec.Name
	if segName == "" {
		segName = SegmentNameFromFile(path.Base(key))
	}
	segType := spec.Type
	if segType == "" {
		segType = "cdp_imported"
	}
	visibility := spec.Visibility
	if visibility == "" {
		visibility = "dsp_private"
	}
	// Data-party classification (ADR 0009): the producer snapshots it from the
	// selected provider onto the spec at enqueue; absent that it's first-party
	// (our own upload). Stamped onto the segment + every profile.signal so
	// reporting/targeting/GDPR can key on it without parsing the licence string.
	dataParty := spec.DataParty
	if dataParty == "" {
		dataParty = "first"
	}
	segID, err := p.Audience.UpsertSegment(ctx, accountID, segName, segType, segmentSource(job), visibility)
	if err != nil {
		log.Error("ingest: upsert segment failed (will retry)", "error", err)
		return ingestjobs.IngestResult{}, infraErr{err}
	}
	if err := p.Audience.SetSegmentProvenance(ctx, accountID, segID, job.ProviderID, dataParty); err != nil {
		// Non-fatal: provenance is attribution metadata, not the membership
		// itself — a failure here shouldn't retry the whole import.
		log.Warn("ingest: set segment provenance failed", "segment", segID, "error", err)
	}
	added, err := p.Audience.AddMembers(ctx, accountID, segID, values)
	if err != nil {
		log.Error("ingest: add members failed (will retry)", "segment", segID, "error", err)
		return ingestjobs.IngestResult{}, infraErr{err}
	}
	// The audience_segment_members trigger (migration 078) appends these adds to
	// the change-log in the same write, so the append-based cache picks them up —
	// no explicit changelog call here.

	matched := 0
	if p.Matcher != nil {
		if matched, err = p.Matcher.CountKnownIdentifiers(ctx, values); err != nil {
			log.Error("ingest: match-rate query failed", "error", err)
			matched = 0
		} else if err := p.Audience.SetSegmentUploadStats(ctx, accountID, segID, len(values), matched); err != nil {
			log.Error("ingest: persist match rate failed", "error", err)
		}
	}

	// Publish the ProfileSignalEvent (chunked over NATS). Reporting lands it in
	// ClickHouse — where the profile-builder reconciles from (ADR 0006 phase 2).
	access := spec.Access
	if access == "" {
		access = defaultAccess(job)
	}
	source := signalSource(job)
	if p.Bus != nil && len(ids) > 0 {
		pub := events.NewPublisher(p.Bus, p.Log)
		for start := 0; start < len(ids); start += signalChunk {
			end := start + signalChunk
			if end > len(ids) {
				end = len(ids)
			}
			ev := events.ProfileSignalEvent{
				SchemaVersion: events.CurrentSchemaVersion,
				// trace_id: the real request trace ONLY (upload request when inline;
				// empty when the async worker has no request). ingest_trace_id: the
				// always-present, distinct-format batch lineage (never confusable).
				TraceID:       tracing.TraceIDFromContext(ctx),
				IngestTraceID: ingestJobTrace(job),
				AccountID:     accountID,
				Provider:      provider,
				ProviderID:    job.ProviderID,
				DataParty:     dataParty,
				Source:        source,
				Access:        access,
				SegmentID:     segID,
				SegmentName:   segName,
				Visibility:    visibility,
				Consent:       spec.Consent,
				ObservedAt:    started,
				IDs:           ids[start:end],
			}
			if err := pub.PublishJSON(ctx, events.SubjectProfileSignal, ev); err != nil {
				p.Log.Error("ingest: profile signal publish failed",
					"segment", segID, "chunk_start", start, "error", err)
			}
		}
	}

	if p.Bus != nil {
		payload, _ := json.Marshal(events.AudienceInvalidateEvent{
			SchemaVersion: events.CurrentSchemaVersion,
			Source:        "ingest",
			SegmentID:     segID,
			AccountID:     accountID,
		})
		if err := p.Bus.Publish(ctx, events.SubjectCacheInvalidateAudience, payload); err != nil {
			p.Log.Warn("ingest: invalidate publish failed", "segment", segID, "error", err)
		}
	}

	res, err := p.finishFile(ctx, bucket, provider, key, accountID, started, segID, matched, result, rejectedKey, "")
	if err != nil {
		return ingestjobs.IngestResult{}, err
	}
	res.MembersAdded = added
	log.Info("ingest: file processed", "segment", segID, "segment_name", segName,
		"valid", len(result.Valid), "rejected", len(result.Quarantine),
		"added", added, "matched", matched)
	return res, nil
}

// segmentSource is the audience_segments.source label — dropzone:{provider} for
// the drop-zone, the caller-declared source (e.g. crm_upload/portal_csv) for
// gateway uploads.
func segmentSource(job ingestjobs.Job) string {
	if job.Source == ingestjobs.SourceDropzone {
		return "dropzone:" + job.Provider
	}
	if job.SegmentSpec.Access != "" {
		return job.SegmentSpec.Access
	}
	return "crm_upload"
}

// signalSource is the ProfileSignalEvent.Source — "dropzone" for the drop-zone,
// else the API source label.
func signalSource(job ingestjobs.Job) string {
	if job.Source == ingestjobs.SourceDropzone {
		return "dropzone"
	}
	return job.Source
}

// defaultAccess is the licence stamped on signals when the spec doesn't set one.
func defaultAccess(job ingestjobs.Job) string {
	if job.Source == ingestjobs.SourceDropzone {
		return "purchased:" + job.Provider
	}
	return "first_party"
}

// rejectedPrefix / processedPrefix bucket keys under a provider folder for the
// drop-zone (which is per-provider) and under an "api/" folder for uploads
// (which have no provider).
func rejectedPrefix(provider string) string {
	if provider == "" {
		return "api/rejected/"
	}
	return provider + "/rejected/"
}

func processedPrefix(provider string) string {
	if provider == "" {
		return "api/processed/"
	}
	return provider + "/processed/"
}

// finishFile moves the source out of incoming/ and returns the terminal counts.
// errMsg == "" means the file processed cleanly; a non-empty message means a
// content failure (the file is moved to rejected/). The caller records the run
// on the audience_ingest_jobs row (MarkDone for a clean run, MarkFailed with
// errMsg for a content failure). Returns an infraErr if the source move fails
// (retryable — nothing was recorded).
func (p *Processor) finishFile(ctx context.Context, bucket, provider, key, accountID string, started time.Time,
	segID string, matched int, result pipeline.Result, rejectedKey, errMsg string,
) (ingestjobs.IngestResult, error) {
	dest := processedPrefix(provider) + path.Base(key)
	if errMsg != "" {
		dest = rejectedPrefix(provider) + path.Base(key)
	}
	if err := p.moveObject(ctx, bucket, key, dest); err != nil {
		p.Log.Error("ingest: move file failed (will retry)", "file", key, "error", err)
		return ingestjobs.IngestResult{}, infraErr{err}
	}
	rate := 0.0
	if n := len(result.Valid); n > 0 {
		rate = float64(matched) / float64(n)
	}
	return ingestjobs.IngestResult{
		SegmentID: segID, TotalRows: result.Stats.TotalInput, ValidRows: len(result.Valid),
		RejectedRows: len(result.Quarantine), MatchedRows: matched, MatchRate: rate,
		RejectedKey: rejectedKey,
	}, nil
}

// quarantineStaged is the content-REJECT path inside Process (undecodable / no
// data rows): it quarantines the whole file (move to rejected/ + an .error.txt
// marker) and returns a rejectErr so the job is marked FAILED with the reason,
// not retried and not silently "done".
func (p *Processor) quarantineStaged(ctx context.Context, bucket, provider, key, accountID string, started time.Time, reason string) (ingestjobs.IngestResult, error) {
	base := path.Base(key)
	rejectedKey := rejectedPrefix(provider) + base
	if _, err := p.finishFile(ctx, bucket, provider, key, accountID, started, "", 0, pipeline.Result{}, rejectedKey, reason); err != nil {
		return ingestjobs.IngestResult{}, err // infra move failure → retry
	}
	marker := rejectedKey + ".error.txt"
	if err := p.Objects.Put(ctx, bucket, marker, strings.NewReader(reason), int64(len(reason)), "text/plain"); err != nil {
		p.Log.Warn("ingest: quarantine marker write failed", "file", key, "error", err)
	}
	p.Log.Error("ingest: file rejected", "provider", provider, "file", key, "reason", reason)
	// REJECT: fail the job with the reason (shown in the monitor / returned 422
	// on the sync path), not marked done.
	return ingestjobs.IngestResult{RejectedKey: rejectedKey}, rejectErr{reason}
}

// buildMappings merges the default id-column mappings with the spec's overrides
// (lowercased — headers are lowercased at decode, so mapping keys must match)
// and resolves the required-field list. Shared by Process and ValidateSample so
// the pre-flight sample check uses the EXACT same rules as the real run.
func buildMappings(spec ingestjobs.SegmentSpec) (map[string]string, []string) {
	mappings := map[string]string{}
	for k, v := range defaultIDMappings {
		mappings[strings.ToLower(k)] = v
	}
	for k, v := range spec.FieldMappings {
		mappings[strings.ToLower(k)] = v
	}
	required := spec.RequiredFields
	if len(required) == 0 {
		required = []string{"id_value"}
	}
	return mappings, required
}

// ValidateSample is the fast PRE-FLIGHT for the synchronous upload path: decode
// name/body, sample up to n data rows, apply the SAME field-mapping + required-
// field validation as Process, and return an error if the file can't be parsed
// or NONE of the sampled rows yields a valid id_value. The gateway calls this
// BEFORE staging so a wrong-shaped file is rejected with a 422 up front instead
// of being discovered mid-ingest.
func (p *Processor) ValidateSample(ctx context.Context, name string, body []byte, spec ingestjobs.SegmentSpec, n int) error {
	// ADR 0008: decrypt before decode, same as Process, so a PGP-encrypted file
	// we can't read is rejected up front on the sync upload path (422) rather
	// than being staged and discovered mid-ingest.
	body, wasEncrypted, derr := pgp.MaybeDecrypt(body, p.PGPKeyring)
	if derr != nil {
		return Reject(pgpRejectReason)
	}
	// ADR 0009: reject a cleartext file from an encryption_expected provider up
	// front (422), same rule the processor enforces at process time.
	if spec.EncryptionExpected && !wasEncrypted {
		return Reject(encryptionRequiredReason)
	}
	records, err := DecodeFile(ctx, name, body)
	if err != nil {
		return Reject(fmt.Sprintf("could not parse file: %v", err))
	}
	if len(records) == 0 {
		return Reject("file has no data rows")
	}
	if len(records) > n {
		records = records[:n]
	}
	mappings, required := buildMappings(spec)
	result := p.Pipeline.Process(ctx, records, pipeline.PublisherConfig{
		PublisherID: "sample", Format: "csv", RequiredFields: required, FieldMappings: mappings,
	})
	if len(result.Valid) == 0 {
		cols := make([]string, 0, len(records[0]))
		for c := range records[0] {
			cols = append(cols, c)
		}
		sort.Strings(cols)
		return Reject(fmt.Sprintf("no usable id column in the first %d row(s) — need one of [%s]; file has columns [%s]",
			len(records), strings.Join(idColumnNames(), ", "), strings.Join(cols, ", ")))
	}
	// Atomic per-file: a bad row in the sample means the whole file would be
	// rejected at process time — so reject at upload too (unless the threshold
	// tolerates it).
	total := len(result.Valid) + len(result.Quarantine)
	if len(result.Quarantine) > 0 && len(result.Quarantine)*100 > p.MaxRejectPct*total {
		return Reject(fmt.Sprintf("%d of the first %d row(s) failed validation — file rejected, all-or-nothing%s (raise ingest.max_reject_pct to allow partial imports)",
			len(result.Quarantine), total, firstQuarantineDetail(result.Quarantine)))
	}
	return nil
}

// firstQuarantineDetail returns a short " (e.g. …)" clause naming the first
// failing row's column + reason, so a reject message says WHY, not just a count.
func firstQuarantineDetail(q []pipeline.QuarantineRecord) string {
	if len(q) == 0 || len(q[0].Errors) == 0 {
		return ""
	}
	e := q[0].Errors[0]
	return fmt.Sprintf(" (e.g. column %q: %s)", e.Field, e.Message)
}

// idColumnNames lists the default source columns that map to id_value, for a
// helpful rejection message.
func idColumnNames() []string {
	cols := make([]string, 0, len(defaultIDMappings))
	for k := range defaultIDMappings {
		cols = append(cols, k)
	}
	sort.Strings(cols)
	return cols
}

// DefaultIDColumns returns the (lowercased) source column names that map to
// id_value by default — user_id/id/hashed_email/email_sha256/uid2/device_id.
// The mapping-builder (ADR 0008) uses it to SUGGEST an id_value mapping when a
// sample column looks like an id.
func DefaultIDColumns() []string { return idColumnNames() }

func (p *Processor) readObject(ctx context.Context, bucket, key string) ([]byte, error) {
	rc, err := p.Objects.Get(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (p *Processor) moveObject(ctx context.Context, bucket, from, to string) error {
	body, err := p.readObject(ctx, bucket, from)
	if err != nil {
		return fmt.Errorf("read %s: %w", from, err)
	}
	if err := p.Objects.Put(ctx, bucket, to, bytes.NewReader(body), int64(len(body)), "text/csv"); err != nil {
		return fmt.Errorf("put %s: %w", to, err)
	}
	if err := p.Objects.Delete(ctx, bucket, from); err != nil {
		return fmt.Errorf("delete %s: %w", from, err)
	}
	return nil
}

// writeRejected persists quarantined rows as CSV with an _errors column — the
// durable quarantine (pkg/pipeline's Result.Quarantine is in-memory).
func (p *Processor) writeRejected(ctx context.Context, bucket, key string, rows []pipeline.QuarantineRecord) error {
	body, err := renderRejectedCSV(rows)
	if err != nil {
		return err
	}
	return p.Objects.Put(ctx, bucket, key, bytes.NewReader(body), int64(len(body)), "text/csv")
}

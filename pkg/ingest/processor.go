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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"time"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

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
// nothing, because the run never happened. A CONTENT outcome (bad file, no
// valid rows) returns (result, nil): the file is processed (quarantined) and
// the job is done.
type infraErr struct{ err error }

func (e infraErr) Error() string { return e.err.Error() }

// IsInfra reports whether err is a retryable infra failure (unwrapped via
// errors.As). Callers use it to decide retry-vs-fail.
func IsInfra(err error) bool {
	var i infraErr
	return errors.As(err, &i)
}

// Process is the single audience-ingest processor: it reads the staged file at
// job.FileBucket/job.FileKey, runs decode → field-map → validate/normalise →
// CreateSegment/AddMembers → match rate → publish profile.signal in chunks →
// quarantine rejects, and returns the terminal counts. It moves the source file
// out of incoming/; the caller records the run on the audience_ingest_jobs row
// via MarkDone (the terminal counts fold the old onboarding_runs table, ADR 0007
// Phase 4). An INFRA error is returned wrapped so the caller retries; content
// failures return a result with nil error.
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
	records, err := DecodeFile(ctx, path.Base(key), body)
	if err != nil {
		return p.quarantineStaged(ctx, bucket, provider, key, accountID, started, fmt.Sprintf("decode: %v", err))
	}
	if len(records) == 0 {
		return p.quarantineStaged(ctx, bucket, provider, key, accountID, started, "no data rows")
	}

	mappings := map[string]string{}
	for k, v := range defaultIDMappings {
		mappings[k] = v
	}
	for k, v := range spec.FieldMappings {
		mappings[k] = v
	}
	required := spec.RequiredFields
	if len(required) == 0 {
		required = []string{"id_value"}
	}
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
		return p.finishFile(ctx, bucket, provider, key, accountID, started, "", 0, result, rejectedKey,
			"no valid rows after validation")
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
	segID, err := p.Audience.UpsertSegment(ctx, accountID, segName, segType, segmentSource(job), visibility)
	if err != nil {
		log.Error("ingest: upsert segment failed (will retry)", "error", err)
		return ingestjobs.IngestResult{}, infraErr{err}
	}
	added, err := p.Audience.AddMembers(ctx, accountID, segID, values)
	if err != nil {
		log.Error("ingest: add members failed (will retry)", "segment", segID, "error", err)
		return ingestjobs.IngestResult{}, infraErr{err}
	}

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
				TraceID:       tracing.TraceIDFromContext(ctx),
				AccountID:     accountID,
				Provider:      provider,
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
		payload := []byte(`{"segment_id":"` + segID + `","account_id":"` + accountID + `"}`)
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

// quarantineStaged is the content-failure path inside Process: it quarantines
// the whole file (finishFile move) and returns a terminal result so the job is
// marked done, not retried.
func (p *Processor) quarantineStaged(ctx context.Context, bucket, provider, key, accountID string, started time.Time, reason string) (ingestjobs.IngestResult, error) {
	base := path.Base(key)
	rejectedKey := rejectedPrefix(provider) + base
	res, err := p.finishFile(ctx, bucket, provider, key, accountID, started, "", 0, pipeline.Result{}, rejectedKey, reason)
	if err != nil {
		return ingestjobs.IngestResult{}, err
	}
	marker := rejectedKey + ".error.txt"
	if err := p.Objects.Put(ctx, bucket, marker, strings.NewReader(reason), int64(len(reason)), "text/plain"); err != nil {
		p.Log.Warn("ingest: quarantine marker write failed", "file", key, "error", err)
	}
	p.Log.Error("ingest: file quarantined", "provider", provider, "file", key, "reason", reason)
	return res, nil
}

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

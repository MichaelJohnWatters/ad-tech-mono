package main

// onboarding.go — the third-party audience DROP-ZONE poller.
//
// Providers (data partners, CDPs, publishers with big files) land CSV files
// in the adtech-onboarding bucket under {provider}/incoming/, next to a
// per-provider manifest.json describing the contract (owning account, id
// type, consent basis, licence/access, field mappings). The poller Lists the
// bucket on an interval (Minio S3-event support isn't assumed), validates +
// normalizes each file through pkg/pipeline, then:
//
//   - writes PG memberships (UpsertSegment + AddMembers; segment name = file
//     name) and the per-upload match rate,
//   - appends normalized rows to the profile_signals Delta table via the
//     datalake sink (same process, same single-writer lock),
//   - PERSISTS rejected rows to {provider}/rejected/{file} (pkg/pipeline's
//     quarantine was previously in-memory only),
//   - moves the processed file to {provider}/processed/{file},
//   - records the run in onboarding_runs (feeds the staff monitor),
//   - publishes the audience cache-invalidate.
//
// Failure split: CONTENT failures (bad manifest, unsupported format, no valid
// rows) quarantine the file and record a failed run — the file never blocks
// the zone. INFRA failures (Postgres/Minio/NATS down) log ERROR and leave the
// file in incoming/ so the next tick retries; nothing is recorded because the
// run never happened.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	pgstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// onboardingSignalChunk caps how many ids ride in one ProfileSignalEvent —
// mirrors cmd/gateway's profileSignalChunk so a huge drop-zone file fans out to
// bounded NATS messages rather than one giant payload.
const onboardingSignalChunk = 1000

// startOnboarding wires the drop-zone poller: object store, Postgres
// (memberships + run records + match rate), NATS (cache invalidates), and
// the interval ticker. Degrades explicitly: no object store or no Postgres
// disables the zone with an ERROR (files would silently pile up otherwise);
// no NATS only disables invalidates (warm caches still refresh on interval).
func startOnboarding(cfg *config.Config, log *slog.Logger, lc *lifecycle.Lifecycle) {
	if !keys.Pipeline.OnboardingEnabled.Get(cfg) {
		return
	}
	obj := connectObjects(cfg, log)
	if obj == nil {
		log.Error("onboarding drop-zone disabled: no object store")
		return
	}
	bucket := keys.Pipeline.OnboardingBucket.Get(cfg)
	if err := obj.EnsureBucket(context.Background(), bucket); err != nil {
		log.Warn("onboarding: ensure bucket failed", "bucket", bucket, "error", err)
	}

	dbURL := cfg.Get(keys.Database.URL.Key(), "")
	db, err := sql.Open("postgres", dbURL)
	if err == nil {
		err = db.Ping()
	}
	if err != nil {
		log.Error("onboarding drop-zone disabled: postgres unavailable", "error", err)
		return
	}
	lc.OnShutdown("onboarding-db", func(_ context.Context) error { return db.Close() })

	jobStore := ingestjobs.NewPostgresIngestStore(db)
	if hn, _ := os.Hostname(); hn != "" {
		jobStore.WorkerID = hn
	} else {
		jobStore.WorkerID = os.Getenv("POD_NAME")
	}
	o := &onboarder{
		obj:     obj,
		bucket:  bucket,
		aud:     audiencepg.New(db),
		matcher: pgstore.NewFromDB(db),
		db:      db,
		pipe:    pipeline.New(log),
		jobs:    jobStore,
		log:     log,
	}
	if bus, err := natsbus.New(keys.Pipeline.NATSURL.Get(cfg), constants.ServicePipeline+"-onboarding", log); err != nil {
		log.Warn("onboarding: nats unavailable — audience invalidates disabled", "error", err)
	} else {
		o.bus = bus
		lc.OnShutdown("onboarding-nats", func(_ context.Context) error { return bus.Close() })
	}

	o.retention = func() time.Duration { return keys.Pipeline.OnboardingRetention.Get(cfg) }
	interval := keys.Pipeline.OnboardingPollEvery.Get(cfg)
	stop := make(chan struct{})
	lc.OnShutdown("onboarding-poller", func(_ context.Context) error { close(stop); return nil })
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		o.tick(context.Background())
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				o.tick(context.Background())
			}
		}
	}()
	log.Info("onboarding drop-zone poller running", "bucket", bucket, "poll_interval", interval.String())

	// The ingest worker (ADR 0007) drains the audience_ingest_jobs queue the
	// poller enqueues into — decoupling enqueue (list + manifest) from process
	// (decode → match → memberships), durable + N-replica safe via SKIP LOCKED.
	startIngestWorker(cfg, o, log, lc)
}

// onboardingManifest is the per-provider contract at {provider}/manifest.json.
type onboardingManifest struct {
	Provider  string `json:"provider"`
	AccountID string `json:"account_id"` // owning tenant for the segments
	// IDType stamped on rows without an explicit id_type column
	// (user_id | hashed_email | uid2 | device_id | household). Default user_id.
	IDType string `json:"id_type"`
	// ConsentBasis: contractual | consent | none. Anything but "none"/""
	// marks the signals consented for personalisation.
	ConsentBasis string `json:"consent_basis"`
	// Access records the licence: purchased:{provider} | barter:{provider} |
	// first_party. Default purchased:{provider}.
	Access string `json:"access"`
	// Visibility of the segments: public | dsp_private. Default dsp_private.
	Visibility string `json:"visibility"`
	// SegmentType for created segments. Default cdp_imported.
	SegmentType string `json:"segment_type"`
	// FieldMappings: provider CSV column → canonical column. Merged over the
	// defaults (user_id/id/hashed_email/uid2/email_sha256 → id_value).
	FieldMappings map[string]string `json:"field_mappings"`
	// RequiredFields validated per row. Default ["id_value"].
	RequiredFields []string `json:"required_fields"`
}

// identityCounter is the match-rate seam (implemented by pkg/store/postgres.Store).
type identityCounter interface {
	CountKnownIdentifiers(ctx context.Context, ids []string) (int, error)
}

type onboarder struct {
	obj     objects.Store
	bucket  string
	aud     *audiencepg.Store
	matcher identityCounter
	db      *sql.DB // onboarding_runs writes
	bus     events.EventBus
	pipe    *pipeline.Pipeline
	jobs    ingestjobs.Store // audience_ingest_jobs queue (ADR 0007)
	log     *slog.Logger
	// retention bounds how long processed/rejected artifact BYTES live in
	// the bucket after ingestion; the onboarding_runs row survives the
	// sweep. A func so the TierLive config key applies on the next tick
	// without a restart.
	retention func() time.Duration
}

// defaultIDMappings normalize the common id column names to id_value so a
// provider whose files already use one of these needs no manifest mapping.
var defaultIDMappings = map[string]string{
	"user_id": "id_value", "id": "id_value", "hashed_email": "id_value",
	"email_sha256": "id_value", "uid2": "id_value", "device_id": "id_value",
}

// idTypeForColumn maps a mapped-away source column to the id_type it implies,
// when the manifest doesn't say and the row has no id_type column.
var idTypeForColumn = map[string]string{
	"hashed_email": "hashed_email", "email_sha256": "hashed_email",
	"uid2": "uid2", "device_id": "device_id",
}

// tick scans {provider}/incoming/ across all providers and ENQUEUES an ingest
// job for every new file (ADR 0007) — the ingest worker processes them. Reading
// the manifest at enqueue keeps the queued job self-describing; the (bucket,key)
// dedupe index means re-listing a file already queued/running is a no-op.
// Sequential — the zone is a batch surface, not a hot path.
func (o *onboarder) tick(ctx context.Context) {
	keys, err := o.obj.List(ctx, o.bucket, "")
	if err != nil {
		o.log.Error("onboarding: list bucket failed", "bucket", o.bucket, "error", err)
		return
	}
	for _, key := range keys {
		parts := strings.Split(key, "/")
		if len(parts) < 3 || parts[1] != "incoming" || parts[len(parts)-1] == "" {
			continue
		}
		o.enqueueFile(ctx, parts[0], key)
	}
	o.sweep(ctx)
}

// enqueueFile reads the provider manifest, builds a SegmentSpec, and enqueues
// one ingest job for the staged file. Manifest failures quarantine the file
// straight away (content failure — no job to run). Infra failures (manifest
// read, enqueue) log ERROR and leave the file in incoming/ for the next tick.
func (o *onboarder) enqueueFile(ctx context.Context, provider, key string) {
	log := o.log.With("provider", provider, "file", key)
	manifest, err := o.loadManifest(ctx, provider)
	if err != nil {
		o.quarantineFile(ctx, provider, key, time.Now().UTC(), fmt.Sprintf("manifest: %v", err))
		return
	}
	spec := ingestjobs.SegmentSpec{
		Name:           segmentNameFromFile(path.Base(key)),
		Type:           manifest.SegmentType,
		Visibility:     manifest.Visibility,
		Consent:        manifest.ConsentBasis != "" && manifest.ConsentBasis != "none",
		IDType:         manifest.IDType,
		FieldMappings:  manifest.FieldMappings,
		RequiredFields: manifest.RequiredFields,
		Access:         manifest.Access,
	}
	id, err := o.jobs.Enqueue(ctx, ingestjobs.Job{
		AccountID:   manifest.AccountID,
		Source:      ingestjobs.SourceDropzone,
		Provider:    provider,
		FileBucket:  o.bucket,
		FileKey:     key,
		SegmentSpec: spec,
	})
	if err != nil {
		log.Error("onboarding: enqueue ingest job failed (will retry)", "error", err)
		return
	}
	if id != "" {
		log.Info("onboarding: file enqueued", "job", id)
	}
}

// sweep deletes processed/rejected artifact bytes for runs older than the
// retention window and stamps swept_at — the run's stats stay queryable in
// the monitor forever, only the object copies go. Without this, processed/
// and rejected/ grow unbounded (the original file is only ever MOVED there,
// never deleted; decompression happens in memory so there is no separate
// unzipped copy to worry about).
func (o *onboarder) sweep(ctx context.Context) {
	if o.db == nil || o.retention == nil {
		return
	}
	retention := o.retention()
	if retention <= 0 {
		return // 0/negative = retention disabled, keep artifacts forever
	}
	rows, err := o.db.QueryContext(ctx, `
SELECT id::text, provider, file_key, COALESCE(rejected_key, '')
FROM onboarding_runs
WHERE swept_at IS NULL AND finished_at < now() - $1::interval
LIMIT 200`, fmt.Sprintf("%f seconds", retention.Seconds()))
	if err != nil {
		o.log.Error("onboarding sweep: query failed", "error", err)
		return
	}
	type target struct{ id, provider, fileKey, rejectedKey string }
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.provider, &t.fileKey, &t.rejectedKey); err == nil {
			targets = append(targets, t)
		}
	}
	rows.Close()

	swept := 0
	for _, t := range targets {
		if err := o.deleteArtifacts(ctx, t.provider, t.fileKey, t.rejectedKey); err != nil {
			// Transient object-store failure — retry next tick (swept_at
			// stays NULL).
			o.log.Error("onboarding sweep: delete failed (will retry)", "file", t.fileKey, "error", err)
			continue
		}
		if _, err := o.db.ExecContext(ctx, `UPDATE onboarding_runs SET swept_at = now() WHERE id = $1::uuid`, t.id); err != nil {
			o.log.Error("onboarding sweep: mark failed", "run", t.id, "error", err)
			continue
		}
		swept++
	}
	if swept > 0 {
		o.log.Info("onboarding sweep: artifacts deleted", "runs", swept, "retention", retention.String())
	}
}

// deleteArtifacts removes every bucket copy a run can have left behind:
// the processed/ copy of the source file, the rejected-rows CSV, and the
// quarantine error marker. Delete is a no-op on missing keys, so the
// quarantined-whole-file case (no processed copy) needs no branching.
func (o *onboarder) deleteArtifacts(ctx context.Context, provider, fileKey, rejectedKey string) error {
	targets := []string{provider + "/processed/" + path.Base(fileKey)}
	if rejectedKey != "" {
		targets = append(targets, rejectedKey, rejectedKey+".error.txt")
	}
	for _, key := range targets {
		if err := o.obj.Delete(ctx, o.bucket, key); err != nil {
			return fmt.Errorf("delete %s: %w", key, err)
		}
	}
	return nil
}

// infraErr marks a retryable INFRA failure — the caller (ingest worker) leaves
// the job for retry and records nothing, because the run never happened. A
// CONTENT outcome (bad file, no valid rows) returns (result, nil): the file is
// processed (quarantined) and the job is done.
type infraErr struct{ err error }

func (e infraErr) Error() string { return e.err.Error() }

// processStagedFile is the single audience-ingest processor (ADR 0007): it
// reads the staged file at job.FileBucket/job.FileKey, runs the SAME stages the
// drop-zone poller used to run inline (decode → field-map → validate/normalise
// → CreateSegment/AddMembers → match rate → publish profile.signal in chunks →
// quarantine rejects), and returns the terminal counts. It records the run in
// onboarding_runs and moves the source file out of incoming/ (kept in Phase 1;
// ADR folds onboarding_runs into the job row in Phase 4). An INFRA error is
// returned as infraErr so the worker retries; content failures return a result
// with nil error.
func (o *onboarder) processStagedFile(ctx context.Context, job ingestjobs.Job) (ingestjobs.IngestResult, error) {
	started := time.Now().UTC()
	provider, key := job.Provider, job.FileKey
	spec := job.SegmentSpec
	accountID := job.AccountID
	log := o.log.With("provider", provider, "file", key, "job", job.ID)

	body, err := o.readObject(ctx, key)
	if err != nil {
		log.Error("onboarding: read file failed (will retry)", "error", err)
		return ingestjobs.IngestResult{}, infraErr{err}
	}
	records, err := decodeOnboardingFile(ctx, path.Base(key), body)
	if err != nil {
		return o.quarantineStaged(ctx, provider, key, accountID, started, fmt.Sprintf("decode: %v", err))
	}
	if len(records) == 0 {
		return o.quarantineStaged(ctx, provider, key, accountID, started, "no data rows")
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

	result := o.pipe.Process(ctx, records, pipeline.PublisherConfig{
		PublisherID:    provider,
		Format:         "csv",
		RequiredFields: required,
		FieldMappings:  mappings,
	})

	// Persist the quarantine FIRST — rejected rows must survive even if the
	// rest of the run fails and retries.
	rejectedKey := ""
	if len(result.Quarantine) > 0 {
		rejectedKey = provider + "/rejected/" + path.Base(key)
		if err := o.writeRejected(ctx, rejectedKey, result.Quarantine); err != nil {
			log.Error("onboarding: persist quarantine failed (will retry)", "error", err)
			return ingestjobs.IngestResult{}, infraErr{err}
		}
	}
	if len(result.Valid) == 0 {
		return o.finishFile(ctx, provider, key, accountID, started, "", 0, result, rejectedKey,
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
		segName = segmentNameFromFile(path.Base(key))
	}
	segType := spec.Type
	if segType == "" {
		segType = "cdp_imported"
	}
	visibility := spec.Visibility
	if visibility == "" {
		visibility = "dsp_private"
	}
	segID, err := o.aud.UpsertSegment(ctx, accountID, segName, segType, "dropzone:"+provider, visibility)
	if err != nil {
		log.Error("onboarding: upsert segment failed (will retry)", "error", err)
		return ingestjobs.IngestResult{}, infraErr{err}
	}
	added, err := o.aud.AddMembers(ctx, accountID, segID, values)
	if err != nil {
		log.Error("onboarding: add members failed (will retry)", "segment", segID, "error", err)
		return ingestjobs.IngestResult{}, infraErr{err}
	}

	matched := 0
	if o.matcher != nil {
		if matched, err = o.matcher.CountKnownIdentifiers(ctx, values); err != nil {
			log.Error("onboarding: match-rate query failed", "error", err)
			matched = 0
		} else if err := o.aud.SetSegmentUploadStats(ctx, accountID, segID, len(values), matched); err != nil {
			log.Error("onboarding: persist match rate failed", "error", err)
		}
	}

	// Publish the same ProfileSignalEvent the gateway audience-upload path
	// publishes (chunked over NATS). Reporting lands it in ClickHouse — where
	// the profile-builder now reconciles from (ADR 0006 phase 2) — and, while
	// the dual-write still runs, the pipeline sink lands the lake copy from the
	// same message.
	access := spec.Access
	if access == "" {
		access = "purchased:" + provider
	}
	if o.bus != nil && len(ids) > 0 {
		pub := events.NewPublisher(o.bus, o.log)
		for start := 0; start < len(ids); start += onboardingSignalChunk {
			end := start + onboardingSignalChunk
			if end > len(ids) {
				end = len(ids)
			}
			ev := events.ProfileSignalEvent{
				SchemaVersion: events.CurrentSchemaVersion,
				TraceID:       tracing.TraceIDFromContext(ctx),
				AccountID:     accountID,
				Provider:      provider,
				Source:        "dropzone",
				Access:        access,
				SegmentID:     segID,
				SegmentName:   segName,
				Visibility:    visibility,
				Consent:       spec.Consent,
				ObservedAt:    started,
				IDs:           ids[start:end],
			}
			if err := pub.PublishJSON(ctx, events.SubjectProfileSignal, ev); err != nil {
				o.log.Error("onboarding: profile signal publish failed",
					"segment", segID, "chunk_start", start, "error", err)
			}
		}
	}

	if o.bus != nil {
		payload := []byte(`{"segment_id":"` + segID + `","account_id":"` + accountID + `"}`)
		if err := o.bus.Publish(ctx, events.SubjectCacheInvalidateAudience, payload); err != nil {
			o.log.Warn("onboarding: invalidate publish failed", "segment", segID, "error", err)
		}
	}

	res, err := o.finishFile(ctx, provider, key, accountID, started, segID, matched, result, rejectedKey, "")
	if err != nil {
		return ingestjobs.IngestResult{}, err
	}
	log.Info("onboarding: file processed", "segment", segID, "segment_name", segName,
		"valid", len(result.Valid), "rejected", len(result.Quarantine),
		"added", added, "matched", matched)
	return res, nil
}

// finishFile moves the source out of incoming/ and records the run. errMsg ==
// "" means completed; a non-empty message records a failed (content) run. It
// returns the IngestResult (counts) the worker records on the job row, or an
// infraErr if the source move fails (retryable — nothing was recorded).
func (o *onboarder) finishFile(ctx context.Context, provider, key, accountID string, started time.Time,
	segID string, matched int, result pipeline.Result, rejectedKey, errMsg string,
) (ingestjobs.IngestResult, error) {
	dest := provider + "/processed/" + path.Base(key)
	if errMsg != "" {
		dest = provider + "/rejected/" + path.Base(key)
	}
	if err := o.moveObject(ctx, key, dest); err != nil {
		o.log.Error("onboarding: move file failed (will retry)", "file", key, "error", err)
		return ingestjobs.IngestResult{}, infraErr{err}
	}
	status, matchRate := "completed", (*float64)(nil)
	if errMsg != "" {
		status = "failed"
	}
	rate := 0.0
	if n := len(result.Valid); n > 0 {
		rate = float64(matched) / float64(n)
		matchRate = &rate
	}
	o.recordRun(ctx, runRow{
		provider: provider, fileKey: key, accountID: accountID, segmentID: segID,
		status: status, total: result.Stats.TotalInput, valid: len(result.Valid),
		rejected: len(result.Quarantine), matched: matched, matchRate: matchRate,
		errMsg: errMsg, rejectedKey: rejectedKey, started: started,
	})
	return ingestjobs.IngestResult{
		SegmentID: segID, TotalRows: result.Stats.TotalInput, ValidRows: len(result.Valid),
		RejectedRows: len(result.Quarantine), MatchedRows: matched, MatchRate: rate,
		RejectedKey: rejectedKey,
	}, nil
}

// quarantineStaged is the content-failure path inside processStagedFile: it
// quarantines the whole file (finishFile move + run record) and returns a
// terminal result so the job is marked done, not retried.
func (o *onboarder) quarantineStaged(ctx context.Context, provider, key, accountID string, started time.Time, reason string) (ingestjobs.IngestResult, error) {
	base := path.Base(key)
	rejectedKey := provider + "/rejected/" + base
	res, err := o.finishFile(ctx, provider, key, accountID, started, "", 0, pipeline.Result{}, rejectedKey, reason)
	if err != nil {
		return ingestjobs.IngestResult{}, err
	}
	marker := rejectedKey + ".error.txt"
	if err := o.obj.Put(ctx, o.bucket, marker, strings.NewReader(reason), int64(len(reason)), "text/plain"); err != nil {
		o.log.Warn("onboarding: quarantine marker write failed", "file", key, "error", err)
	}
	o.log.Error("onboarding: file quarantined", "provider", provider, "file", key, "reason", reason)
	return res, nil
}

// quarantineFile handles CONTENT failures where we couldn't even process
// rows: move the whole file to rejected/ with a .error.txt marker and record
// a failed run, so the zone never wedges on one bad file.
func (o *onboarder) quarantineFile(ctx context.Context, provider, key string, started time.Time, reason string) {
	base := path.Base(key)
	if err := o.moveObject(ctx, key, provider+"/rejected/"+base); err != nil {
		o.log.Error("onboarding: quarantine move failed (will retry)", "file", key, "error", err)
		return
	}
	marker := provider + "/rejected/" + base + ".error.txt"
	if err := o.obj.Put(ctx, o.bucket, marker, strings.NewReader(reason), int64(len(reason)), "text/plain"); err != nil {
		o.log.Warn("onboarding: quarantine marker write failed", "file", key, "error", err)
	}
	o.recordRun(ctx, runRow{
		provider: provider, fileKey: key, status: "failed", errMsg: reason,
		rejectedKey: provider + "/rejected/" + base, started: started,
	})
	o.log.Error("onboarding: file quarantined", "provider", provider, "file", key, "reason", reason)
}

type runRow struct {
	provider, fileKey, accountID, segmentID string
	status                                  string
	total, valid, rejected, matched         int
	matchRate                               *float64
	errMsg, rejectedKey                     string
	started                                 time.Time
}

func (o *onboarder) recordRun(ctx context.Context, r runRow) {
	if o.db == nil {
		return
	}
	const q = `
INSERT INTO onboarding_runs (provider, file_key, account_id, segment_id, status,
    total_rows, valid_rows, rejected_rows, matched_rows, match_rate, error, rejected_key,
    started_at, finished_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''), NULLIF($12, ''), $13, now())`
	accountID := sql.NullString{String: r.accountID, Valid: r.accountID != ""}
	segmentID := sql.NullString{String: r.segmentID, Valid: r.segmentID != ""}
	if _, err := o.db.ExecContext(ctx, q, r.provider, r.fileKey, accountID, segmentID,
		r.status, r.total, r.valid, r.rejected, r.matched, r.matchRate,
		r.errMsg, r.rejectedKey, r.started); err != nil {
		o.log.Error("onboarding: record run failed", "file", r.fileKey, "error", err)
	}
}

func (o *onboarder) loadManifest(ctx context.Context, provider string) (onboardingManifest, error) {
	var m onboardingManifest
	body, err := o.readObject(ctx, provider+"/manifest.json")
	if err != nil {
		return m, fmt.Errorf("read %s/manifest.json: %w", provider, err)
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, fmt.Errorf("parse %s/manifest.json: %w", provider, err)
	}
	if m.AccountID == "" {
		return m, fmt.Errorf("%s/manifest.json: account_id is required", provider)
	}
	return m, nil
}

func (o *onboarder) readObject(ctx context.Context, key string) ([]byte, error) {
	rc, err := o.obj.Get(ctx, o.bucket, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (o *onboarder) moveObject(ctx context.Context, from, to string) error {
	body, err := o.readObject(ctx, from)
	if err != nil {
		return fmt.Errorf("read %s: %w", from, err)
	}
	if err := o.obj.Put(ctx, o.bucket, to, bytes.NewReader(body), int64(len(body)), "text/csv"); err != nil {
		return fmt.Errorf("put %s: %w", to, err)
	}
	if err := o.obj.Delete(ctx, o.bucket, from); err != nil {
		return fmt.Errorf("delete %s: %w", from, err)
	}
	return nil
}

// writeRejected persists quarantined rows as CSV with an _errors column —
// the durable quarantine (pkg/pipeline's Result.Quarantine is in-memory).
func (o *onboarder) writeRejected(ctx context.Context, key string, rows []pipeline.QuarantineRecord) error {
	body, err := renderRejectedCSV(rows)
	if err != nil {
		return err
	}
	return o.obj.Put(ctx, o.bucket, key, bytes.NewReader(body), int64(len(body)), "text/csv")
}

// renderRejectedCSV serializes quarantined rows with a stable column order
// (sorted union of keys) plus a trailing _errors column.
func renderRejectedCSV(rows []pipeline.QuarantineRecord) ([]byte, error) {
	colSet := map[string]bool{}
	for _, r := range rows {
		for k := range r.Record {
			colSet[k] = true
		}
	}
	cols := make([]string, 0, len(colSet))
	for k := range colSet {
		cols = append(cols, k)
	}
	sort.Strings(cols)

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(append(append([]string{}, cols...), "_errors")); err != nil {
		return nil, err
	}
	for _, r := range rows {
		row := make([]string, 0, len(cols)+1)
		for _, c := range cols {
			row = append(row, r.Record[c])
		}
		msgs := make([]string, len(r.Errors))
		for i, e := range r.Errors {
			msgs[i] = e.Error()
		}
		row = append(row, strings.Join(msgs, "; "))
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

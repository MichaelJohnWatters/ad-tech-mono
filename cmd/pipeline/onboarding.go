package main

// onboarding.go — the third-party audience DROP-ZONE poller.
//
// Providers (data partners, CDPs, publishers with big files) land CSV files
// in the adtech-onboarding bucket under {provider}/incoming/, next to a
// per-provider manifest.json describing the contract (owning account, id
// type, consent basis, licence/access, field mappings). The poller Lists the
// bucket on an interval (Minio S3-event support isn't assumed) and ENQUEUES an
// ingest job for every new file (ADR 0007). The shared processor
// (pkg/ingest.Processor) — the same one the gateway upload path runs inline —
// does the decode → validate → normalise → match → AddMembers → publish
// profile.signal → quarantine work when the ingest worker drains the queue.
//
// Failure split at enqueue time: a bad/missing manifest is a CONTENT failure —
// the file is quarantined straight away (there is no job to run). An INFRA
// failure (manifest read, enqueue) logs ERROR and leaves the file in incoming/
// for the next tick.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingest"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	pgstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

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
		obj:    obj,
		bucket: bucket,
		db:     db,
		jobs:   jobStore,
		log:    log,
	}
	// The shared processor (ADR 0007) does the actual decode → match →
	// memberships → publish work; the poller only enqueues. The gateway builds
	// the same Processor from its own connections for inline uploads.
	proc := &ingest.Processor{
		Objects:  obj,
		Audience: audiencepg.New(db),
		Matcher:  pgstore.NewFromDB(db),
		Pipeline: pipeline.New(log),
		DB:       db,
		Log:      log,
	}
	if bus, err := natsbus.New(keys.Pipeline.NATSURL.Get(cfg), constants.ServicePipeline+"-onboarding", log); err != nil {
		log.Warn("onboarding: nats unavailable — audience invalidates disabled", "error", err)
	} else {
		o.bus = bus
		proc.Bus = bus
		lc.OnShutdown("onboarding-nats", func(_ context.Context) error { return bus.Close() })
	}
	o.proc = proc

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

type onboarder struct {
	obj    objects.Store
	bucket string
	db     *sql.DB // onboarding_runs writes (quarantineFile) + sweep queries
	bus    events.EventBus
	jobs   ingestjobs.Store    // audience_ingest_jobs queue (ADR 0007)
	proc   *ingest.Processor   // the shared decode → match → memberships processor
	log    *slog.Logger
	// retention bounds how long processed/rejected artifact BYTES live in
	// the bucket after ingestion; the onboarding_runs row survives the
	// sweep. A func so the TierLive config key applies on the next tick
	// without a restart.
	retention func() time.Duration
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
		Name:           ingest.SegmentNameFromFile(path.Base(key)),
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

// processStagedFile is the poller/worker's thin call into the shared processor
// (ADR 0007). The ingest worker (ingest_worker.go) calls this per claimed job;
// it exists so the worker keeps a stable method on the onboarder while the
// actual logic lives in pkg/ingest, shared with the gateway inline path.
func (o *onboarder) processStagedFile(ctx context.Context, job ingestjobs.Job) (ingestjobs.IngestResult, error) {
	return o.proc.Process(ctx, job)
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

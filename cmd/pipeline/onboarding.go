package main

// onboarding.go — the third-party audience DROP-ZONE poller.
//
// INTERNAL-ONLY (delivery deferred): the drop-zone PROCESSING is real, but it
// presupposes a file already sitting in the adtech-onboarding bucket. We do NOT
// support direct bucket access, so external providers currently have no way to
// self-deliver here — files land via platform-managed S3 creds (staff/ops or an
// internal feed). The customer-facing ingestion path is the authed, tenant-bound
// gateway upload (POST /v1/api/audiences), which also handles big files (202 →
// this same worker). To open the drop-zone to external providers, add a delivery
// broker: presigned prefix-scoped PUT URLs, an authed streaming upload, or
// per-provider scoped credentials + per-prefix IAM. See ADR 0007.
//
// Files land in the adtech-onboarding bucket under {provider}/incoming/, next to
// a per-provider manifest.json describing the contract (owning account, id
// type, consent basis, licence/access, field mappings, optional notify_emails).
// The poller Lists the bucket on an interval (Minio S3-event support isn't
// assumed) and ENQUEUES an ingest job for every new file (ADR 0007). The shared
// processor
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
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/email"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events/natsbus"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingest"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pgp"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/secrets"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	pgstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// loadPGPKeyring reads the platform PGP private key(s) directly from the secrets
// table (ADR 0008) and parses them into a decrypt keyring. It loads every
// non-revoked (active + rotating) pgp_private row so a file encrypted to a
// rotating predecessor still decrypts, and applies the same at-rest cipher the
// warm-cache loader uses (passthrough when SECRETS_ENCRYPTION_KEY is unset).
// Returns nil (with a WARN) when no key is configured or all rows are
// unparseable — callers leave the keyring nil, so PGP files are rejected and
// plaintext files ingest as before.
func loadPGPKeyring(db *sql.DB, log *slog.Logger) openpgp.EntityList {
	rows, err := db.Query(
		`SELECT value FROM secrets WHERE purpose = $1 AND status != 'revoked' ORDER BY status = 'active' DESC`,
		secrets.PurposePGPPrivate,
	)
	if err != nil {
		log.Warn("onboarding: pgp key lookup failed — PGP-encrypted files will be rejected", "error", err)
		return nil
	}
	defer rows.Close()

	cipher, err := secrets.NewCipherFromEnv()
	if err != nil {
		log.Error("onboarding: pgp at-rest cipher invalid — PGP-encrypted files will be rejected", "error", err)
		return nil
	}

	var keyring openpgp.EntityList
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			log.Warn("onboarding: pgp key scan failed", "error", err)
			continue
		}
		value, derr := cipher.Decrypt(stored)
		if derr != nil {
			log.Warn("onboarding: pgp key at-rest decrypt failed", "error", derr)
			continue
		}
		kr, perr := pgp.ParsePrivate(value)
		if perr != nil {
			log.Warn("onboarding: parse pgp private key failed", "error", perr)
			continue
		}
		keyring = append(keyring, kr...)
	}
	if len(keyring) == 0 {
		log.Warn("onboarding: no active pgp_private secret — PGP-encrypted files will be rejected")
		return nil
	}
	return keyring
}

// startOnboarding wires the drop-zone poller: object store, Postgres
// (the audience_ingest_jobs queue + memberships + match rate), NATS (cache
// invalidates), and the interval ticker. Degrades explicitly: no object store or no Postgres
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
	// Ingest completion emails (ADR 0008 Feature 3): the async worker notifies a
	// job's recipients when it finishes. SMTP when configured, else an in-memory
	// sender that only logs (dev).
	emailFrom := keys.Pipeline.EmailFrom.Get(cfg)
	o := &onboarder{
		obj:         obj,
		bucket:      bucket,
		db:          db,
		jobs:        jobStore,
		log:         log,
		emailSender: connectIngestEmail(cfg, emailFrom, log),
		emailFrom:   emailFrom,
	}
	// The shared processor (ADR 0007) does the actual decode → match →
	// memberships → publish work; the poller only enqueues. The gateway builds
	// the same Processor from its own connections for inline uploads.
	proc := &ingest.Processor{
		Objects:      obj,
		Audience:     audiencepg.New(db),
		Matcher:      pgstore.NewFromDB(db),
		Pipeline:     pipeline.New(log),
		Log:          log,
		MaxRejectPct: keys.Pipeline.IngestMaxRejectPct.Get(cfg),
	}
	// ADR 0008: load the platform PGP private key so drop-zone files encrypted
	// to the platform public key decrypt on ingest. A small direct DB read at
	// construction (cheaper than a whole secrets warm-cache here); a rotation
	// lands on the next pipeline restart. Absent/unparseable = WARN + PGP files
	// rejected as content failures; plaintext files are unaffected.
	if kr := loadPGPKeyring(db, log); kr != nil {
		proc.PGPKeyring = kr
		log.Info("onboarding: PGP decrypt-on-ingest enabled")
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
	// NotifyEmails (optional, ADR 0008 Feature 3) are recipients emailed when a
	// job for this provider's files reaches a terminal state (done/failed).
	// Empty = no email.
	NotifyEmails []string `json:"notify_emails"`
}

type onboarder struct {
	obj    objects.Store
	bucket string
	db     *sql.DB // audience_ingest_jobs sweep (swept_at stamp) queries
	bus    events.EventBus
	jobs   ingestjobs.Store  // audience_ingest_jobs queue (ADR 0007)
	proc   *ingest.Processor // the shared decode → match → memberships processor
	log    *slog.Logger
	// emailSender + emailFrom deliver ingest completion emails (ADR 0008
	// Feature 3) after MarkDone/MarkFailed. A nil sender (or a job with no
	// NotifyEmails) is a no-op — email is best-effort.
	emailSender email.Sender
	emailFrom   string
	// retention bounds how long processed/rejected artifact BYTES live in
	// the bucket after ingestion; the audience_ingest_jobs row survives the
	// sweep (stamped swept_at). A func so the TierLive config key applies on
	// the next tick without a restart.
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
		AccountID:    manifest.AccountID,
		Source:       ingestjobs.SourceDropzone,
		Provider:     provider,
		FileBucket:   o.bucket,
		FileKey:      key,
		SegmentSpec:  spec,
		NotifyEmails: ingest.SanitizeNotifyEmails(manifest.NotifyEmails),
	})
	if err != nil {
		log.Error("onboarding: enqueue ingest job failed (will retry)", "error", err)
		return
	}
	if id != "" {
		log.Info("onboarding: file enqueued", "trace_id", ingestjobs.TraceForID(id), "job", id)
	}
}

// processStagedFile is the poller/worker's thin call into the shared processor
// (ADR 0007). The ingest worker (ingest_worker.go) calls this per claimed job;
// it exists so the worker keeps a stable method on the onboarder while the
// actual logic lives in pkg/ingest, shared with the gateway inline path.
func (o *onboarder) processStagedFile(ctx context.Context, job ingestjobs.Job) (ingestjobs.IngestResult, error) {
	return o.proc.Process(ctx, job)
}

// sweep deletes processed/rejected artifact bytes for terminal ingest jobs
// older than the retention window and stamps swept_at on the job row — the
// job's stats stay queryable in the monitor forever, only the object copies
// go. Without this, processed/ and rejected/ grow unbounded (the original file
// is only ever MOVED there, never deleted; decompression happens in memory so
// there is no separate unzipped copy to worry about). ADR 0007 Phase 4 folded
// onboarding_runs into audience_ingest_jobs — the sweep now stamps the job row.
func (o *onboarder) sweep(ctx context.Context) {
	if o.db == nil || o.retention == nil {
		return
	}
	retention := o.retention()
	if retention <= 0 {
		return // 0/negative = retention disabled, keep artifacts forever
	}
	// The sweep spans every tenant's ingest jobs (no account filter), so under
	// the NOBYPASSRLS app role (security #77) it goes through the platform read
	// hatch — otherwise RLS blanks the SELECT and the aged jobs never get swept.
	pg := pgstore.NewFromDB(o.db)
	rows, closeRows, err := pg.QueryPlatform(ctx, `
SELECT id::text, COALESCE(provider, ''), file_key, COALESCE(rejected_key, '')
FROM audience_ingest_jobs
WHERE swept_at IS NULL AND status IN ('done', 'failed')
  AND finished_at IS NOT NULL AND finished_at < now() - $1::interval
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
	closeRows()

	swept := 0
	for _, t := range targets {
		if err := o.deleteArtifacts(ctx, t.provider, t.fileKey, t.rejectedKey); err != nil {
			// Transient object-store failure — retry next tick (swept_at
			// stays NULL).
			o.log.Error("onboarding sweep: delete failed (will retry)", "file", t.fileKey, "error", err)
			continue
		}
		if _, err := pg.ExecPlatform(ctx, `UPDATE audience_ingest_jobs SET swept_at = now() WHERE id = $1::uuid`, t.id); err != nil {
			o.log.Error("onboarding sweep: mark failed", "job", t.id, "error", err)
			continue
		}
		swept++
	}
	if swept > 0 {
		o.log.Info("onboarding sweep: artifacts deleted", "jobs", swept, "retention", retention.String())
	}
}

// deleteArtifacts removes every bucket copy a job can have left behind:
// the processed/ copy of the source file, the rejected-rows CSV, and the
// quarantine error marker. Delete is a no-op on missing keys, so the
// quarantined-whole-file case (no processed copy) needs no branching. The
// processed/ prefix mirrors pkg/ingest: {provider}/processed/ for drop-zone
// jobs, api/processed/ for gateway uploads (empty provider).
func (o *onboarder) deleteArtifacts(ctx context.Context, provider, fileKey, rejectedKey string) error {
	processedPrefix := provider + "/processed/"
	if provider == "" {
		processedPrefix = "api/processed/"
	}
	targets := []string{processedPrefix + path.Base(fileKey)}
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

// quarantineFile handles CONTENT failures at ENQUEUE time — a bad/missing
// manifest, before any job row exists (and, for a manifest failure, before we
// can even resolve the owning account). It moves the whole file to rejected/
// with a .error.txt marker and logs ERROR so the zone never wedges on one bad
// file. There is no audience_ingest_jobs row to record against here: the
// manifest failure is what prevents enqueue (account_id is NOT NULL on the job
// table), so this path is object-store quarantine + ERROR log only.
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
	o.log.Error("onboarding: file quarantined", "provider", provider, "file", key, "reason", reason)
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

// connectIngestEmail selects SMTP (Mailpit/SES) when pipeline.smtp_host is set,
// else an in-memory sender that only logs deliveries (dev). Mirrors
// cmd/report-runner's connectEmail.
func connectIngestEmail(cfg *config.Config, from string, log *slog.Logger) email.Sender {
	host := keys.Pipeline.SMTPHost.Get(cfg)
	if host == "" {
		log.Info("pipeline ingest email via memory sender (no smtp_host set) — deliveries are logged only")
		return email.NewMemory(log)
	}
	if user := keys.Pipeline.SMTPUsername.Get(cfg); user != "" {
		log.Info("pipeline ingest email via authenticated SMTP", "host", host, "username", user)
		return email.NewSMTPAuth(host, from, user, keys.Pipeline.SMTPPassword.Get(cfg), log)
	}
	log.Info("pipeline ingest email via SMTP", "host", host)
	return email.NewSMTP(host, from, log)
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

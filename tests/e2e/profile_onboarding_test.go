//go:build e2e

// Profile Store Phase 1 gates — onboarding into profile_signals.
//
//  1. Portal CSV upload (multipart): match rate reported from identity_graph,
//     memberships written, segment targetable in a real auction, and the
//     normalized rows land in the profile_signals Delta table.
//  2. Drop-zone: a provider CSV + manifest in the adtech-onboarding bucket is
//     polled by the pipeline, memberships appear, bad rows persist to
//     rejected/, the run is recorded, and the segment wins an auction.
package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pgp"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestProfileOnboardingCSVUpload(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "profile-csv")

	uniq := fmt.Sprintf("pcsv-%d", time.Now().UnixNano())
	users := []string{uniq + "-u1", uniq + "-u2", uniq + "-u3", uniq + "-u4"}
	// Half the list is known to the identity graph → match rate 0.5.
	h.SeedIdentityEdges(t, users[0], users[1])

	csv := "user_id\n" + strings.Join(users, "\n") + "\n"
	res := h.UploadAudienceCSV(t, w.AdvAcc.ID, uniq+"-list", "public", csv)
	if res.SegmentID == "" {
		t.Fatal("upload returned empty segment id")
	}
	if res.MembersSent != len(users) || res.MembersAdded != len(users) {
		t.Errorf("members sent/added = %d/%d, want %d/%d", res.MembersSent, res.MembersAdded, len(users), len(users))
	}
	if res.Matched != 2 || res.MatchRate < 0.49 || res.MatchRate > 0.51 {
		t.Errorf("matched=%d match_rate=%.2f, want 2 / 0.50", res.Matched, res.MatchRate)
	}
	if got := h.SegmentMemberCount(t, res.SegmentID); got != len(users) {
		t.Errorf("segment members = %d, want %d", got, len(users))
	}
	// Match rate persisted on the segment row.
	var persisted float64
	if err := h.DB.QueryRow(`SELECT match_rate FROM audience_segments WHERE id = $1`, res.SegmentID).Scan(&persisted); err != nil {
		t.Errorf("read persisted match_rate: %v", err)
	} else if persisted < 0.49 || persisted > 0.51 {
		t.Errorf("persisted match_rate = %.2f, want 0.50", persisted)
	}

	// The segment is targetable: only members win the campaign.
	setTargeting(t, h, w.Campaign.ID, "include_segments", pq.StringArray{res.SegmentID})
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{})
	setTargeting(t, h, w.Campaign.ID, "include_device", pq.StringArray{})
	h.RefreshAllCaches(t)

	// Member (u3 — a member the identity graph does NOT know; membership is
	// what gates, not graph presence).
	res1 := h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "USA", Device: "mobile", UserID: users[2],
	})
	if win := h.ExtractWinner(t, res1); win.NoBid || win.CampaignID != w.Campaign.ID {
		t.Errorf("member auction: winner=%+v, want campaign %s", win, w.Campaign.ID)
	}
	// Non-member → no-bid.
	res2 := h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "USA", Device: "mobile", UserID: uniq + "-stranger",
	})
	if win := h.ExtractWinner(t, res2); !win.NoBid {
		t.Errorf("non-member auction won (%+v), want no-bid", win)
	}

	// The normalized rows reached the analytical store: NATS → reporting →
	// ClickHouse (the single source of truth since ADR 0006 phase 5; the
	// profile-builder + hourly Parquet export both derive from it). account_id
	// is unique per run (reset truncates), so this count is just this upload.
	deadline := time.Now().Add(45 * time.Second)
	countQ := fmt.Sprintf("SELECT count() FROM adtech.profile_signals WHERE account_id='%s'", w.AdvAcc.ID)
	for {
		if got := h.ClickHouseScalar(t, countQ); got >= len(users) {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("profile_signals in clickhouse = %d, want >= %d", h.ClickHouseScalar(t, countQ), len(users))
			break
		}
		time.Sleep(2 * time.Second)
	}
}

// TestAudienceUploadReject — a wrong-shaped file (no id column) is rejected up
// front with 422 + a reason (the sample pre-flight), while a valid file 200s.
// Case-insensitive headers: an UPPERCASE id column is accepted (ADR 0007).
func TestAudienceUploadReject(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "aud-reject")

	// No id column → 422 with a reason.
	code, body := h.UploadAudienceCSVStatus(t, w.AdvAcc.ID, "bad-shape", "public", "email_address,city\nfoo@bar.com,NYC\n")
	if code != 422 {
		t.Fatalf("no-id upload: status %d, want 422 (body: %s)", code, body)
	}
	if !strings.Contains(strings.ToLower(body), "reject") || !strings.Contains(strings.ToLower(body), "id column") {
		t.Errorf("422 body should explain the reject reason, got: %s", body)
	}

	// UPPERCASE id header is accepted (headers are lowercased at decode).
	res := h.UploadAudienceCSV(t, w.AdvAcc.ID, "upper-ok", "public", "USER_ID\n"+w.AdvAcc.ID+"-u1\n")
	if res.SegmentID == "" || res.MembersAdded != 1 {
		t.Errorf("uppercase-header upload: %+v, want 1 member added", res)
	}
}

// TestAudienceCustomMapping — a file with non-standard column names is rejected
// without a mapping, but imports once a saved mapping points its column at
// id_value (ADR 0008); mappings are tenant-isolated (validated in unit tests).
func TestAudienceCustomMapping(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "aud-map")

	// "crm_ref" is not one of our default id columns → rejected without a mapping.
	csv := "crm_ref,region\n" + w.AdvAcc.ID + "-m1,EU\n" + w.AdvAcc.ID + "-m2,US\n"
	code, _ := h.UploadAudienceCSVStatus(t, w.AdvAcc.ID, "map-nomap", "public", csv)
	if code != 422 {
		t.Errorf("unmapped unusual-column file: status %d, want 422", code)
	}

	// Save a mapping crm_ref → id_value, then the same file imports.
	mapID := h.CreateAudienceMapping(t, w.AdvAcc.ID, "acme-crm",
		map[string]string{"crm_ref": "id_value"}, "user_id")
	if mapID == "" {
		t.Fatal("create mapping returned no id")
	}
	code, body := h.UploadAudienceCSVMapped(t, w.AdvAcc.ID, "map-ok", "public", csv, mapID)
	if code != 200 {
		t.Fatalf("mapped upload: status %d, want 200 (%s)", code, body)
	}
	if !strings.Contains(body, `"members_added":2`) {
		t.Errorf("mapped upload should import 2 members, got: %s", body)
	}
}

// TestAudienceIngestEmail — an upload notifies its additional recipients on
// both success and failure (ADR 0008), delivered via Mailpit.
func TestAudienceIngestEmail(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "aud-email")
	uniq := time.Now().UnixNano()
	okAddr := fmt.Sprintf("notify-ok-%d@e2e.local", uniq)
	failAddr := fmt.Sprintf("notify-fail-%d@e2e.local", uniq)

	// Success → "succeeded" email to the additional recipient.
	code, body := h.UploadAudienceCSVNotify(t, w.AdvAcc.ID, "email-ok", "public",
		"user_id\n"+w.AdvAcc.ID+"-e1\n", []string{okAddr})
	if code != 200 {
		t.Fatalf("ok upload: status %d (%s)", code, body)
	}
	harness.WaitFor(t, 45*time.Second, "success email delivered", func() bool {
		for _, m := range h.MailpitSearch(t, okAddr) {
			if strings.Contains(strings.ToLower(m.Subject), "succeed") {
				return true
			}
		}
		return false
	})

	// Failure (no id column) → "failed" email to the additional recipient.
	code, _ = h.UploadAudienceCSVNotify(t, w.AdvAcc.ID, "email-bad", "public",
		"email_address,city\nfoo@bar.com,NYC\n", []string{failAddr})
	if code != 422 {
		t.Fatalf("bad upload: status %d, want 422", code)
	}
	harness.WaitFor(t, 45*time.Second, "failure email delivered", func() bool {
		for _, m := range h.MailpitSearch(t, failAddr) {
			if strings.Contains(strings.ToLower(m.Subject), "fail") {
				return true
			}
		}
		return false
	})
}

// TestAudiencePGPUpload — a file PGP-encrypted to the platform public key is
// decrypted on ingest and imported; a file encrypted to a DIFFERENT key is
// rejected with 422 (ADR 0008).
func TestAudiencePGPUpload(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "pgp")

	pub := h.PGPPublicKey(t, w.AdvAcc.ID)
	csv := "user_id\n" + w.AdvAcc.ID + "-p1\n" + w.AdvAcc.ID + "-p2\n"

	// Encrypted to the platform key → decrypts + imports.
	enc, err := pgp.Encrypt([]byte(csv), pub)
	if err != nil {
		t.Fatalf("encrypt to platform key: %v", err)
	}
	res := h.UploadAudienceCSV(t, w.AdvAcc.ID, "pgp-ok", "public", string(enc))
	if res.SegmentID == "" || res.MembersAdded != 2 {
		t.Errorf("pgp upload result %+v, want 2 members added", res)
	}

	// Encrypted to a DIFFERENT key → we can't decrypt → rejected.
	_, otherPub, _, err := pgp.Generate()
	if err != nil {
		t.Fatalf("gen other key: %v", err)
	}
	encBad, err := pgp.Encrypt([]byte(csv), otherPub)
	if err != nil {
		t.Fatalf("encrypt to other key: %v", err)
	}
	code, body := h.UploadAudienceCSVStatus(t, w.AdvAcc.ID, "pgp-bad", "public", string(encBad))
	if code != 422 {
		t.Errorf("wrong-key pgp upload: status %d, want 422 (%s)", code, body)
	}
}

// TestOnboardingRetentionSweep — processed/rejected artifact bytes are
// deleted once their ingest job ages past pipeline.onboarding_retention (30d
// default): a backdated audience_ingest_jobs row makes the live poller sweep
// on its next tick, the objects disappear, and the job row survives stamped
// swept_at. ADR 0007 Phase 4 folded onboarding_runs into audience_ingest_jobs,
// so the sweep now stamps the job row (terminal status 'done', not 'completed').
func TestOnboardingRetentionSweep(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	obj, bucket := h.OnboardingBucket(t)
	ctx := context.Background()
	provider := fmt.Sprintf("sweep-%d", time.Now().UnixNano())
	acc := h.CreateAdvertiser(t, "sweep-adv")

	put := func(key, body string) {
		t.Helper()
		if err := obj.Put(ctx, bucket, key, strings.NewReader(body), int64(len(body)), "text/csv"); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	put(provider+"/processed/old.csv", "user_id\nu1\n")
	put(provider+"/rejected/old.csv", "user_id,_errors\n,missing\n")
	put(provider+"/rejected/old.csv.error.txt", "reason")

	// A backdated terminal job: bytes live in {provider}/processed + rejected,
	// finished_at is older than the retention window, swept_at is NULL.
	var runID string
	if err := h.DB.QueryRow(`
INSERT INTO audience_ingest_jobs
    (account_id, source, provider, file_bucket, file_key, segment_spec, rejected_key,
     status, started_at, finished_at)
VALUES ($1::uuid, 'dropzone', $2, $3, $4, '{}'::jsonb, $5,
     'done', now() - interval '31 days', now() - interval '31 days')
RETURNING id::text`, acc.ID, provider, bucket, provider+"/incoming/old.csv",
		provider+"/rejected/old.csv").Scan(&runID); err != nil {
		t.Fatalf("seed old job: %v", err)
	}

	deadline := time.Now().Add(45 * time.Second)
	for {
		var sweptAt *time.Time
		if err := h.DB.QueryRow(`SELECT swept_at FROM audience_ingest_jobs WHERE id = $1::uuid`, runID).Scan(&sweptAt); err != nil {
			t.Fatalf("read swept_at: %v", err)
		}
		if sweptAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poller never swept the aged job")
		}
		time.Sleep(3 * time.Second)
	}
	for _, key := range []string{provider + "/processed/old.csv", provider + "/rejected/old.csv", provider + "/rejected/old.csv.error.txt"} {
		if ok, _ := obj.Exists(ctx, bucket, key); ok {
			t.Errorf("%s survived the retention sweep", key)
		}
	}
}

func TestProfileOnboardingDropZone(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "profile-dz")

	obj, bucket := h.OnboardingBucket(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	provider := fmt.Sprintf("acme-%d", uniq)
	segName := fmt.Sprintf("dz-seg-%d", uniq)
	users := []string{provider + "-u1", provider + "-u2", provider + "-u3"}

	manifest := fmt.Sprintf(`{
		"provider": %q, "account_id": %q, "id_type": "user_id",
		"consent_basis": "contractual", "access": "purchased:acme",
		"visibility": "public", "segment_type": "cdp_imported"
	}`, provider, w.AdvAcc.ID)
	put := func(key, body string) {
		t.Helper()
		if err := obj.Put(ctx, bucket, key, strings.NewReader(body), int64(len(body)), "text/csv"); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	put(provider+"/manifest.json", manifest)
	// All rows valid — strict ingestion (ADR 0007) imports the whole file or
	// rejects it. This one imports; the bad file below proves the reject path.
	csv := "user_id,geo\n" + users[0] + ",US\n" + users[1] + ",GB\n" + users[2] + ",US\n"
	put(provider+"/incoming/"+segName+".csv", csv)
	// A file with ONE bad row (empty id) → the WHOLE file is rejected: no
	// segment, a failed ingest job with a reason.
	badSeg := segName + "-bad"
	put(provider+"/incoming/"+badSeg+".csv", "user_id,geo\n"+provider+"-b1,US\n,GB\n")

	// Second file: gzip-compressed TSV — exercises the multi-format
	// auto-detect (magic-byte sniff + decompress + delimiter sniff).
	gzUsers := []string{provider + "-gz1", provider + "-gz2"}
	gzSegName := segName + "-gz"
	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	_, _ = zw.Write([]byte("user_id\tgeo\n" + gzUsers[0] + "\tUS\n" + gzUsers[1] + "\tGB\n"))
	_ = zw.Close()
	if err := obj.Put(ctx, bucket, provider+"/incoming/"+gzSegName+".tsv.gz",
		bytes.NewReader(gzBuf.Bytes()), int64(gzBuf.Len()), "application/gzip"); err != nil {
		t.Fatalf("put gz file: %v", err)
	}

	// The poller runs on an interval (10s in the local stack); wait for the
	// segment + memberships to appear.
	var segID string
	deadline := time.Now().Add(90 * time.Second)
	for segID == "" {
		_ = h.DB.QueryRow(`SELECT id::text FROM audience_segments WHERE account_id = $1::uuid AND name = $2`,
			w.AdvAcc.ID, segName).Scan(&segID)
		if segID != "" && h.SegmentMemberCount(t, segID) >= len(users) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drop-zone segment %q never materialised (segID=%q)", segName, segID)
		}
		time.Sleep(3 * time.Second)
	}
	if got := h.SegmentMemberCount(t, segID); got != len(users) {
		t.Errorf("drop-zone members = %d, want %d", got, len(users))
	}

	// Source file moved out of incoming/ (won't be reprocessed).
	if ok, _ := obj.Exists(ctx, bucket, provider+"/incoming/"+segName+".csv"); ok {
		t.Error("incoming file still present after processing")
	}
	// The all-good file's ingest job is done with 0 rejects (ADR 0007 folded
	// onboarding_runs into audience_ingest_jobs; terminal status 'done').
	var status string
	var rejected int
	if err := h.DB.QueryRow(`SELECT status, COALESCE(rejected_rows,0) FROM audience_ingest_jobs WHERE provider = $1 AND file_key = $2 ORDER BY created_at DESC LIMIT 1`,
		provider, provider+"/incoming/"+segName+".csv").Scan(&status, &rejected); err != nil {
		t.Errorf("audience_ingest_jobs row missing: %v", err)
	} else if status != "done" || rejected != 0 {
		t.Errorf("job status=%s rejected=%d, want done/0", status, rejected)
	}

	// The BAD file (one empty-id row) is rejected WHOLE: a failed job with a
	// reason, and no segment created (nothing imported — atomic per file).
	badDeadline := time.Now().Add(45 * time.Second)
	for {
		var st, reason string
		err := h.DB.QueryRow(`SELECT status, COALESCE(error,'') FROM audience_ingest_jobs WHERE provider=$1 AND file_key=$2 ORDER BY created_at DESC LIMIT 1`,
			provider, provider+"/incoming/"+badSeg+".csv").Scan(&st, &reason)
		if err == nil && st == "failed" {
			if reason == "" {
				t.Error("rejected job has no reason")
			}
			break
		}
		if time.Now().After(badDeadline) {
			t.Fatalf("bad file never rejected (status=%q)", st)
		}
		time.Sleep(3 * time.Second)
	}
	var badSegCount int
	_ = h.DB.QueryRow(`SELECT count(*) FROM audience_segments WHERE account_id=$1::uuid AND name=$2`, w.AdvAcc.ID, badSeg).Scan(&badSegCount)
	if badSegCount != 0 {
		t.Errorf("rejected file created a segment (%d) — nothing should import", badSegCount)
	}

	// The gzipped TSV materialized its own segment (stacked extensions
	// stripped from the name).
	var gzSegID string
	gzDeadline := time.Now().Add(60 * time.Second)
	for gzSegID == "" || h.SegmentMemberCount(t, gzSegID) < len(gzUsers) {
		_ = h.DB.QueryRow(`SELECT id::text FROM audience_segments WHERE account_id = $1::uuid AND name = $2`,
			w.AdvAcc.ID, gzSegName).Scan(&gzSegID)
		if time.Now().After(gzDeadline) {
			t.Fatalf("gz drop-zone segment %q never materialised (segID=%q)", gzSegName, gzSegID)
		}
		time.Sleep(3 * time.Second)
	}

	// Onboarded segment is targetable in a real auction.
	setTargeting(t, h, w.Campaign.ID, "include_segments", pq.StringArray{segID})
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{})
	setTargeting(t, h, w.Campaign.ID, "include_device", pq.StringArray{})
	h.RefreshAllCaches(t)

	res := h.RunAuctionWith(t, harness.AuctionParams{
		Placement: w.Placement.ExternalID, Geo: "USA", Device: "mobile", UserID: users[0],
	})
	if win := h.ExtractWinner(t, res); win.NoBid || win.CampaignID != w.Campaign.ID {
		t.Errorf("drop-zone member auction: winner=%+v, want campaign %s", win, w.Campaign.ID)
	}
}

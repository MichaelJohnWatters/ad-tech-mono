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

// TestOnboardingRetentionSweep — processed/rejected artifact bytes are
// deleted once their run ages past pipeline.onboarding_retention (30d
// default): a backdated run row makes the live poller sweep on its next
// tick, the objects disappear, and the run row survives stamped swept_at.
func TestOnboardingRetentionSweep(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	obj, bucket := h.OnboardingBucket(t)
	ctx := context.Background()
	provider := fmt.Sprintf("sweep-%d", time.Now().UnixNano())

	put := func(key, body string) {
		t.Helper()
		if err := obj.Put(ctx, bucket, key, strings.NewReader(body), int64(len(body)), "text/csv"); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	put(provider+"/processed/old.csv", "user_id\nu1\n")
	put(provider+"/rejected/old.csv", "user_id,_errors\n,missing\n")
	put(provider+"/rejected/old.csv.error.txt", "reason")

	var runID string
	if err := h.DB.QueryRow(`
INSERT INTO onboarding_runs (provider, file_key, rejected_key, status, started_at, finished_at)
VALUES ($1, $2, $3, 'completed', now() - interval '31 days', now() - interval '31 days')
RETURNING id::text`, provider, provider+"/incoming/old.csv", provider+"/rejected/old.csv").Scan(&runID); err != nil {
		t.Fatalf("seed old run: %v", err)
	}

	deadline := time.Now().Add(45 * time.Second)
	for {
		var sweptAt *time.Time
		if err := h.DB.QueryRow(`SELECT swept_at FROM onboarding_runs WHERE id = $1::uuid`, runID).Scan(&sweptAt); err != nil {
			t.Fatalf("read swept_at: %v", err)
		}
		if sweptAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poller never swept the aged run")
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
	// 3 good rows + 1 bad (empty id → quarantined).
	csv := "user_id,geo\n" + users[0] + ",US\n" + users[1] + ",GB\n" + users[2] + ",US\n,US\n"
	put(provider+"/incoming/"+segName+".csv", csv)

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

	// Quarantine persisted: the bad row is a CSV object under rejected/.
	if ok, err := obj.Exists(ctx, bucket, provider+"/rejected/"+segName+".csv"); err != nil || !ok {
		t.Errorf("rejected-rows object missing (ok=%v err=%v)", ok, err)
	}
	// Source file moved out of incoming/ (won't be reprocessed).
	if ok, _ := obj.Exists(ctx, bucket, provider+"/incoming/"+segName+".csv"); ok {
		t.Error("incoming file still present after processing")
	}
	// Run recorded for the staff monitor.
	var status string
	var rejected int
	if err := h.DB.QueryRow(`SELECT status, rejected_rows FROM onboarding_runs WHERE provider = $1 AND file_key = $2 ORDER BY finished_at DESC LIMIT 1`,
		provider, provider+"/incoming/"+segName+".csv").Scan(&status, &rejected); err != nil {
		t.Errorf("onboarding_runs row missing: %v", err)
	} else if status != "completed" || rejected != 1 {
		t.Errorf("run status=%s rejected=%d, want completed/1", status, rejected)
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

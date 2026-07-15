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

	lakeBefore := h.LakeRows(t, "profile_signals")

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

	// The normalized rows landed in the lake (NATS → pipeline sink; snapshot
	// forces a flush, so this converges within the consume interval).
	deadline := time.Now().Add(45 * time.Second)
	for {
		if got := h.LakeRows(t, "profile_signals"); got >= lakeBefore+len(users) {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("profile_signals lake rows = %d, want >= %d", h.LakeRows(t, "profile_signals"), lakeBefore+len(users))
			break
		}
		time.Sleep(2 * time.Second)
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
	if err := h.DB.QueryRow(`SELECT status, rejected_rows FROM onboarding_runs WHERE provider = $1 ORDER BY finished_at DESC LIMIT 1`,
		provider).Scan(&status, &rejected); err != nil {
		t.Errorf("onboarding_runs row missing: %v", err)
	} else if status != "completed" || rejected != 1 {
		t.Errorf("run status=%s rejected=%d, want completed/1", status, rejected)
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

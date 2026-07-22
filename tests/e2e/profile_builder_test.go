//go:build e2e

// Profile Store Phase 3 gate — a behavioural rule flips a targeting decision,
// with person-level enrollment expanded across the identity cluster:
//
//  1. userA browses (3 consented requests) and is identity-linked to deviceA2.
//  2. The profile-builder enrolls the PERSON into a rule segment, expanding
//     the membership to deviceA2 — which never generated any behaviour.
//  3. A campaign targeting the segment wins for deviceA2 and no-bids for a
//     stranger.
//  4. Tightening the rule and re-running the builder prunes the members
//     (replace-by-segment), flipping the same auction back to no-bid.
package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/profilebuilder"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

// pipelineLakeBucket matches PIPELINE_DATALAKE_BUCKET in k8s/base/pipeline —
// the lake the behaviour/profile signals landed in.
const pipelineLakeBucket = "adtech-datalake-hotcold"

func lakeStore(t *testing.T, h *harness.Harness) datalake.Store {
	t.Helper()
	obj, err := objs3.New(objs3.Config{
		Endpoint:  h.URLs.MinioEndpt,
		AccessKey: "adtech",
		SecretKey: "adtech-local-dev",
		Region:    "us-east-1",
		UseSSL:    false,
	})
	if err != nil {
		t.Fatalf("minio connect: %v", err)
	}
	return datalake.NewObjectStore(obj, pipelineLakeBucket, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func runBuilder(t *testing.T, h *harness.Harness, lake datalake.Store) profilebuilder.Result {
	t.Helper()
	// Read behaviour/profile signals from ClickHouse — where they live since ADR
	// 0006 (the Delta dual-write is retired, so the lake behaviour tables are
	// empty). Mirrors how the batch-conductor wires the builder.
	q, err := profilebuilder.NewCHBehaviourQuerier(profilebuilder.CHConfig{
		Addrs:    []string{routes.DefaultClickHouseNativeAddr},
		Database: "adtech", Username: "adtech", Password: "adtech-local-dev",
	})
	if err != nil {
		t.Fatalf("clickhouse behaviour querier: %v", err)
	}
	defer q.Close()
	res, err := profilebuilder.Run(context.Background(), profilebuilder.Config{
		DB:        h.DB,
		Lake:      lake,
		Behaviour: q,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("profilebuilder.Run: %v", err)
	}
	return res
}

func TestBehaviouralRuleFlipsTargeting(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "profile-rule")
	uniq := time.Now().UnixNano()
	userA := fmt.Sprintf("rule-user-%d", uniq)
	deviceA2 := fmt.Sprintf("rule-dev2-%d", uniq)
	stranger := fmt.Sprintf("rule-stranger-%d", uniq)

	// userA and deviceA2 are the same person (deterministic CRM-match edge).
	h.AddIdentityEdge(t, userA, deviceA2, "cross_device")

	// Rule segment: ≥3 requests on this world's publisher.
	segName := fmt.Sprintf("rule-seg-%d", uniq)
	var segID string
	if err := h.DB.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, visibility, rule)
VALUES ($1::uuid, $2, 'behavioral', 'active', 'profile_builder', 'public',
        jsonb_build_object('event', 'request', 'publisher_id', $3::text, 'min_count', 3))
RETURNING id::text`, w.AdvAcc.ID, segName, w.Publisher.ID).Scan(&segID); err != nil {
		t.Fatalf("create rule segment: %v", err)
	}

	// userA browses: 3 consented requests → behaviour_signals rows.
	for i := 0; i < 3; i++ {
		h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: userA,
		})
	}
	deadline := time.Now().Add(45 * time.Second)
	for h.SignalResidual(t, userA)["behaviour_signals"] < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("behaviour rows never landed: %v", h.SignalResidual(t, userA))
		}
		time.Sleep(2 * time.Second)
	}

	// Builder run: cluster + enroll + expand.
	lake := lakeStore(t, h)
	res := runBuilder(t, h, lake)
	if res.Clusters < 1 {
		t.Fatalf("no clusters built: %+v", res)
	}
	// Assert specific memberships, not totals — the lake keeps behaviour
	// history across e2e runs (PG resets, the lake doesn't), so a prior
	// run's users can legitimately qualify too.
	isMember := func(userID string) bool {
		var n int
		if err := h.DB.QueryRow(`SELECT count(*) FROM audience_segment_members WHERE segment_id = $1 AND user_id = $2`,
			segID, userID).Scan(&n); err != nil {
			t.Fatalf("membership query: %v", err)
		}
		return n > 0
	}
	if !isMember(userA) {
		t.Fatal("userA not enrolled by rule evaluation")
	}
	if !isMember(deviceA2) {
		t.Fatal("deviceA2 not expanded into the segment (cluster expansion)")
	}

	// The segment gates the campaign; deviceA2 (no behaviour of its own) wins
	// via cluster expansion, a stranger doesn't.
	setTargeting(t, h, w.Campaign.ID, "include_segments", pq.StringArray{segID})
	setTargeting(t, h, w.Campaign.ID, "include_geo", pq.StringArray{})
	setTargeting(t, h, w.Campaign.ID, "include_device", pq.StringArray{})
	h.RefreshAllCaches(t)

	winFor := func(userID string) bool {
		res := h.RunAuctionWith(t, harness.AuctionParams{
			Placement: w.Placement.ExternalID, Geo: "GBR", Device: "mobile", UserID: userID,
		})
		win := h.ExtractWinner(t, res)
		return !win.NoBid && win.CampaignID == w.Campaign.ID
	}
	if !winFor(deviceA2) {
		t.Error("deviceA2 (cluster sibling) should win via expanded membership")
	}
	if winFor(stranger) {
		t.Error("stranger won — rule segment gates should exclude non-members")
	}

	// Flip the rule to unsatisfiable → replace-by-segment prunes → the same
	// auction no-bids. (The extra requests fired above don't reach 100.)
	if _, err := h.DB.Exec(`
UPDATE audience_segments
   SET rule = jsonb_set(rule, '{min_count}', '100')
 WHERE id = $1`, segID); err != nil {
		t.Fatalf("tighten rule: %v", err)
	}
	res = runBuilder(t, h, lake)
	if res.Pruned < 2 {
		t.Errorf("prune removed %d members, want ≥2", res.Pruned)
	}
	if isMember(userA) || isMember(deviceA2) {
		t.Error("members survived the tightened rule — replace-by-segment prune failed")
	}
	h.RefreshAllCaches(t)
	if winFor(userA) {
		t.Error("userA still wins after the rule tightened — prune didn't flip targeting")
	}
}

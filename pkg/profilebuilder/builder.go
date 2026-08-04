// Package profilebuilder is the profile store's batch expansion engine —
// the write-time half of the two-expansion-strategies design (the DSP's
// read-time BFS is the freshness top-up). One Run executes three jobs:
//
//  1. CLUSTERING — union-find connected components over identity_graph
//     (min-confidence gate, households excluded, mega-cluster guard) →
//     identity_clusters, rebuilt wholesale in Postgres (serving copy) and
//     as a Delta artifact in the lake (replayable record).
//  2. SEGMENTATION — behavioural rules (audience_segments.rule JSONB)
//     evaluated over behaviour_signals; enrollment is at PERSON level, then
//     expanded to every id in the person's cluster; replace-by-segment prune
//     drops users who no longer qualify. Plain (onboarded) segments get the
//     expansion pass only — every member's cluster siblings join, nothing is
//     pruned.
//  3. RECONCILE — replay profile_signals rows into PG memberships
//     (crash/replay safety: the durable record is the source; a membership
//     lost between upload and crash is restored here).
//
// Jobs 2 and 3 read via Config.Behaviour (server-side ClickHouse GROUP BY,
// ADR 0006 phase 2 — the OOM fix); with Behaviour nil they fall back to
// aggregating the behaviour_signals / profile_signals LAKE tables in Go (the
// original path). Clustering (Job 1) and the identity_clusters lake WRITE are
// unchanged either way.
//
// cmd/profile-builder wires this to a K8s CronJob (dayboundary pattern);
// e2e runs it in-process against the live stack.
package profilebuilder

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
	pgstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// ClustersLakeTable is the Delta artifact the builder publishes each run.
const ClustersLakeTable = "identity_clusters"

var clustersLakeSchema = datalake.Schema{Version: 1, Columns: []datalake.Column{
	{Name: "person_id", Type: "string", Nullable: true},
	{Name: "member_id", Type: "string", Nullable: true},
	{Name: "computed_at", Type: "timestamp", Nullable: true},
}}

// Config wires a Run. Lake and Bus are optional (nil = skip the lake
// artifact / the cache invalidates); DB is required.
type Config struct {
	DB   *sql.DB
	Lake datalake.Store  // identity_clusters writes; behaviour/profile READS only when Behaviour is nil (fallback)
	Bus  events.EventBus // audience cache invalidates
	Log  *slog.Logger

	// Behaviour (ADR 0006 phase 2) moves the three lake READS — behavioural
	// rule evaluation, lookalike category signals, and the reconcile pass —
	// onto server-side ClickHouse GROUP BY, killing the profile-builder's
	// OOM-by-design. Nil falls back to the original in-Go lake aggregation
	// (existing callers/tests and a safety valve keep working). Lake WRITES
	// (the identity_clusters artifact) stay on the lake regardless.
	Behaviour BehaviourQuerier

	MinConfidence  float64 // identity edges below this don't link (default 0.5)
	MaxClusterSize int     // clusters above this are dropped as pathological (default 100)
	Now            time.Time
}

// Result is the per-run outcome, logged and returned for e2e assertions.
type Result struct {
	Clusters        int // multi-member clusters materialized
	ClusterMembers  int
	DroppedClusters int // over MaxClusterSize — linking pathology guard
	RuleSegments    int
	Enrolled        int // memberships added by rule evaluation (post-expansion)
	Pruned          int // memberships removed by replace-by-segment
	Expanded        int // memberships added to plain segments via cluster expansion
	Reconciled      int // memberships restored from profile_signals replay
	WindowDays      int // behaviour-lake read window (max rule window + slack)
}

// Run executes the three jobs in order. Clustering failure aborts (jobs 2/3
// depend on the cluster map); a per-segment failure in jobs 2/3 is logged
// and skipped so one bad rule can't wedge the whole run (replace-by-window
// idempotency: the next run recomputes everything anyway).
func Run(ctx context.Context, cfg Config) (Result, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.MinConfidence <= 0 {
		cfg.MinConfidence = 0.5
	}
	if cfg.MaxClusterSize <= 0 {
		cfg.MaxClusterSize = 100
	}
	now := cfg.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var res Result

	// --- Job 1: clustering ---
	adj, err := pgstore.NewFromDB(cfg.DB).LoadIdentityGraph(ctx)
	if err != nil {
		return res, fmt.Errorf("load identity graph: %w", err)
	}
	clusters, dropped := buildClusters(adj, cfg.MinConfidence, cfg.MaxClusterSize)
	res.Clusters = len(clusters.Members)
	res.ClusterMembers = len(clusters.PersonOf)
	res.DroppedClusters = dropped
	if dropped > 0 {
		log.Warn("profile-builder: oversized clusters dropped (linking pathology guard)",
			"dropped", dropped, "max_cluster_size", cfg.MaxClusterSize)
	}
	if err := persistClustersPG(ctx, cfg.DB, clusters, now); err != nil {
		return res, fmt.Errorf("persist clusters: %w", err)
	}
	if cfg.Lake != nil {
		if err := persistClustersLake(ctx, cfg.Lake, clusters, now); err != nil {
			// The PG serving copy is written; the lake artifact is the
			// replayable record — fail loudly but don't abort segmentation.
			log.Error("profile-builder: lake clusters artifact write failed", "error", err)
		}
	}

	aud := audiencepg.New(cfg.DB)
	changed := map[string]string{} // segment id → account id, for invalidates

	// --- Job 2a: behavioural rules (+ derived: composite/lookalike) ---
	// A failed read FAILS the run rather than silently skipping rule
	// evaluation: skipping would leave stale members (users who no longer
	// qualify keep being targeted) with nothing but a log line to notice.
	// The CronJob retries; memberships are recomputed wholesale anyway.
	//
	// ADR 0006 phase 2: with cfg.Behaviour set, rule evaluation + lookalike
	// category signals are server-side ClickHouse GROUP BY (no raw rows in
	// Go). Nil falls back to the in-Go lake aggregation below.
	if cfg.Behaviour != nil {
		// The window still bounds lookalike's category-signal read; the
		// per-rule QualifyingUsers query applies each rule's own window.
		if window, werr := MaxRuleWindowDays(ctx, cfg.DB); werr != nil {
			log.Error("profile-builder: max rule window query failed; lookalike reads default window", "error", werr)
			res.WindowDays = 30
		} else {
			res.WindowDays = window
		}
		if err := runRuleSegments(ctx, cfg.DB, aud, cfg.Behaviour, clusters, nil, now, log, &res, changed); err != nil {
			return res, err
		}
	} else if cfg.Lake != nil {
		// Windowed read: only partitions inside the widest rule window are
		// fetched (the lake keeps history forever; rules never look past
		// their window, so the builder shouldn't read past it either). A
		// failed window query falls back to an unbounded read — correct,
		// just unpruned.
		filter := datalake.Filter{}
		if window, werr := MaxRuleWindowDays(ctx, cfg.DB); werr != nil {
			log.Error("profile-builder: max rule window query failed; reading unwindowed", "error", werr)
		} else {
			filter.TimeFrom = now.AddDate(0, 0, -window)
			res.WindowDays = window
		}
		rows, err := cfg.Lake.Read(ctx, "behaviour_signals", filter)
		if err != nil {
			return res, fmt.Errorf("read behaviour_signals: %w", err)
		}
		if err := runRuleSegments(ctx, cfg.DB, aud, nil, clusters, rows, now, log, &res, changed); err != nil {
			return res, err
		}
	}

	// --- Job 2b: cluster expansion for plain (onboarded) segments ---
	if err := expandPlainSegments(ctx, cfg.DB, aud, clusters, log, &res, changed); err != nil {
		return res, err
	}

	// --- Job 3: reconcile profile_signals → PG memberships ---
	// ADR 0006 phase 2: server-side GROUP BY over ClickHouse when Behaviour is
	// set (this was the worst offender — an UNWINDOWED whole-table lake read);
	// nil falls back to the lake replay.
	if cfg.Behaviour != nil {
		if err := reconcileFromQuerier(ctx, cfg.Behaviour, aud, log, &res, changed); err != nil {
			log.Error("profile-builder: reconcile failed", "error", err)
		}
	} else if cfg.Lake != nil {
		if err := reconcileProfileSignals(ctx, cfg.Lake, aud, log, &res, changed); err != nil {
			log.Error("profile-builder: reconcile failed", "error", err)
		}
	}

	if cfg.Bus != nil {
		for segID, accountID := range changed {
			payload, _ := json.Marshal(events.AudienceInvalidateEvent{
				SchemaVersion: events.CurrentSchemaVersion,
				Source:        "profile-builder",
				SegmentID:     segID,
				AccountID:     accountID,
			})
			if err := cfg.Bus.Publish(ctx, events.SubjectCacheInvalidateAudience, payload); err != nil {
				log.Warn("profile-builder: invalidate publish failed", "segment", segID, "error", err)
			}
		}
	}

	log.Info("profile-builder run complete",
		"clusters", res.Clusters, "cluster_members", res.ClusterMembers, "dropped_clusters", res.DroppedClusters,
		"rule_segments", res.RuleSegments, "enrolled", res.Enrolled, "pruned", res.Pruned,
		"expanded", res.Expanded, "reconciled", res.Reconciled)
	return res, nil
}

// persistClustersPG rebuilds the identity_clusters serving copy wholesale in
// one transaction — readers never see a half-built state.
func persistClustersPG(ctx context.Context, db *sql.DB, c Clusters, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// DELETE, not TRUNCATE: this runs under the least-privilege adtech_app role
	// (it has DELETE but not TRUNCATE) when the builder is invoked in-process —
	// e.g. the staff onboarding demo runs it inside the gateway. identity_clusters
	// is a small, global, wholesale-rebuilt serving copy, so a full DELETE inside
	// the same tx is equivalent (readers still never see a half-built state) and
	// keeps the builder runnable without granting the app role TRUNCATE.
	if _, err := tx.ExecContext(ctx, `DELETE FROM identity_clusters`); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO identity_clusters (person_id, member_id, computed_at) VALUES ($1, $2, $3)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for pid, members := range c.Members {
		for _, m := range members {
			if _, err := stmt.ExecContext(ctx, pid, m, now); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// persistClustersLake publishes the run's cluster artifact: purge the prior
// snapshot, append the new one. The builder is this table's ONLY writer
// (the pipeline never touches it), so the Delta versions can't race —
// with one caveat: a MANUAL run (Tilt escape hatch) concurrent with the
// conductor's chain step is two processes on one table. The lake writer's
// refuse-to-overwrite commit guard downgrades that from silent corruption
// to one loudly-failed run (retry next chain interval); the CronJob's
// concurrencyPolicy: Forbid covers the scheduled path.
func persistClustersLake(ctx context.Context, lake datalake.Store, c Clusters, now time.Time) error {
	if _, err := lake.PurgeRows(ctx, ClustersLakeTable, func(datalake.Record) bool { return true }); err != nil {
		return fmt.Errorf("clear prior artifact: %w", err)
	}
	var recs []datalake.Record
	for pid, members := range c.Members {
		for _, m := range members {
			recs = append(recs, datalake.Record{"person_id": pid, "member_id": m, "computed_at": now})
		}
	}
	if len(recs) == 0 {
		return nil
	}
	return lake.Write(ctx, ClustersLakeTable, recs, clustersLakeSchema)
}

// expandKeys maps enrolled user keys to the full member set: each key's
// person expands to every id in the cluster; keys outside any cluster stay
// as themselves.
func expandKeys(c Clusters, keys []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, k := range keys {
		if pid, ok := c.PersonOf[k]; ok {
			for _, m := range c.Members[pid] {
				add(m)
			}
		} else {
			add(k)
		}
	}
	return out
}

// runRuleSegments evaluates behavioural rules then derived (composite +
// lookalike) rules. When q != nil the behavioural + lookalike inputs come from
// ClickHouse (server-side GROUP BY, ADR 0006 phase 2); when q == nil they come
// from the in-Go lake rows (fallback). Exactly one of q / rows is used.
func runRuleSegments(ctx context.Context, db *sql.DB, aud *audiencepg.Store, q BehaviourQuerier, clusters Clusters,
	rows []datalake.Record, now time.Time, log *slog.Logger, res *Result, changed map[string]string,
) error {
	segs, err := db.QueryContext(ctx,
		`SELECT id::text, account_id::text, name, rule FROM audience_segments
		  WHERE rule IS NOT NULL AND status = 'active'`)
	if err != nil {
		return fmt.Errorf("list rule segments: %w", err)
	}
	defer segs.Close()
	type ruleSeg struct{ id, accountID, name, raw string }
	var list []ruleSeg
	for segs.Next() {
		var s ruleSeg
		if err := segs.Scan(&s.id, &s.accountID, &s.name, &s.raw); err != nil {
			return fmt.Errorf("scan rule segment: %w", err)
		}
		list = append(list, s)
	}
	if err := segs.Err(); err != nil {
		return err
	}

	// Two passes: behavioural rules first, then DERIVED kinds (composite +
	// lookalike) so they see this run's behavioural output. A derived rule
	// referencing another derived segment sees the previous run's members
	// (single derived pass — documented on CompositeRule).
	apply := func(s ruleSeg, members []string) {
		added, err := aud.AddMembers(ctx, s.accountID, s.id, members)
		if err != nil {
			log.Error("profile-builder: enroll failed — segment skipped", "segment", s.id, "error", err)
			return
		}
		pruned, err := aud.RemoveMembersNotIn(ctx, s.accountID, s.id, members)
		if err != nil {
			log.Error("profile-builder: prune failed", "segment", s.id, "error", err)
			return
		}
		res.Enrolled += added
		res.Pruned += pruned
		if added > 0 || pruned > 0 {
			changed[s.id] = s.accountID
		}
	}

	var derived []ruleSeg
	for _, s := range list {
		res.RuleSegments++
		switch RuleKind([]byte(s.raw)) {
		case "", "behaviour":
			rule, err := ParseRule([]byte(s.raw))
			if err != nil {
				log.Error("profile-builder: invalid rule — segment skipped", "segment", s.id, "name", s.name, "error", err)
				continue
			}
			if rule.Event == "site_visit" {
				// Retargeting rows carry the pixel owner's account — scope
				// so another tenant's identically-named tag can't enroll.
				rule.accountID = s.accountID
			}
			var keys []string
			if q != nil {
				keys, err = q.QualifyingUsers(ctx, rule, now)
				if err != nil {
					log.Error("profile-builder: qualifying-users query failed — segment skipped", "segment", s.id, "name", s.name, "error", err)
					continue
				}
			} else {
				keys = evaluateRule(rows, rule, now)
			}
			apply(s, expandKeys(clusters, keys))
		case "composite", "lookalike":
			derived = append(derived, s)
		default:
			log.Error("profile-builder: unknown rule kind — segment skipped", "segment", s.id, "name", s.name)
		}
	}

	// Lookalike scoring needs a per-person category map. Build it ONCE for the
	// whole derived pass — from ClickHouse category signals (q != nil) or the
	// in-Go lake rows — but only when a lookalike rule actually exists, so a
	// run with no lookalikes skips the read entirely.
	var byPerson map[string]map[string]bool
	byPersonReady := false
	ensureByPerson := func() error {
		if byPersonReady {
			return nil
		}
		if q != nil {
			signals, err := q.CategorySignals(ctx, res.WindowDays, now)
			if err != nil {
				return err
			}
			byPerson = categoriesFromSignals(signals, clusters)
		} else {
			byPerson = personCategories(rows, clusters)
		}
		byPersonReady = true
		return nil
	}

	for _, s := range derived {
		var members []string
		var err error
		switch RuleKind([]byte(s.raw)) {
		case "composite":
			rule, perr := ParseCompositeRule([]byte(s.raw))
			if perr != nil {
				log.Error("profile-builder: invalid composite rule — segment skipped", "segment", s.id, "error", perr)
				continue
			}
			members, err = evaluateComposite(ctx, db, s.accountID, rule, clusters)
		case "lookalike":
			rule, perr := ParseLookalikeRule([]byte(s.raw))
			if perr != nil {
				log.Error("profile-builder: invalid lookalike rule — segment skipped", "segment", s.id, "error", perr)
				continue
			}
			if err = ensureByPerson(); err != nil {
				log.Error("profile-builder: category-signal query failed — lookalike skipped", "segment", s.id, "error", err)
				continue
			}
			members, err = evaluateLookalike(ctx, db, s.accountID, rule, clusters, byPerson)
		}
		if err != nil {
			log.Error("profile-builder: derived rule evaluation failed — segment skipped", "segment", s.id, "error", err)
			continue
		}
		apply(s, members)
	}
	return nil
}

// expandPlainSegments adds cluster siblings to every onboarded (rule-less)
// segment's memberships — the write-time pre-expansion that gives SSP
// stamping cross-device coverage without a bid-path graph walk. Additive
// only: onboarded member rows are ground truth, never pruned here.
func expandPlainSegments(ctx context.Context, db *sql.DB, aud *audiencepg.Store, clusters Clusters,
	log *slog.Logger, res *Result, changed map[string]string,
) error {
	if len(clusters.PersonOf) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx, `
SELECT s.id::text, s.account_id::text, m.user_id
FROM audience_segments s
JOIN audience_segment_members m ON m.segment_id = s.id
WHERE s.rule IS NULL AND s.status = 'active'`)
	if err != nil {
		return fmt.Errorf("list plain memberships: %w", err)
	}
	defer rows.Close()
	type segKey struct{ id, accountID string }
	members := map[segKey][]string{}
	for rows.Next() {
		var k segKey
		var uid string
		if err := rows.Scan(&k.id, &k.accountID, &uid); err != nil {
			return fmt.Errorf("scan membership: %w", err)
		}
		members[k] = append(members[k], uid)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for k, uids := range members {
		expanded := expandKeys(clusters, uids)
		if len(expanded) == len(uids) {
			continue // no cluster brought new ids
		}
		added, err := aud.AddMembers(ctx, k.accountID, k.id, expanded)
		if err != nil {
			log.Error("profile-builder: expansion failed", "segment", k.id, "error", err)
			continue
		}
		if added > 0 {
			res.Expanded += added
			changed[k.id] = k.accountID
		}
	}
	return nil
}

// reconcileProfileSignals replays the onboarding lake rows into PG
// memberships. AddMembers is idempotent, so the common case is a no-op;
// after a crash between upload and membership write (or a PG restore from
// backup), this is what heals the gap.
// reconcileFromQuerier is the ADR 0006 phase 2 reconcile: the (account,
// segment) → id_values grouping runs server-side (groupUniqArray) instead of
// loading the whole profile_signals table into Go. Identical downstream
// semantics to reconcileProfileSignals — AddMembers per (account, segment),
// skip-if-deleted, count restored rows.
func reconcileFromQuerier(ctx context.Context, q BehaviourQuerier, aud *audiencepg.Store,
	log *slog.Logger, res *Result, changed map[string]string,
) error {
	groups, err := q.SegmentMemberships(ctx)
	if err != nil {
		return fmt.Errorf("read profile_signals: %w", err)
	}
	for _, g := range groups {
		if g.AccountID == "" || g.SegmentID == "" || len(g.IDValues) == 0 {
			continue
		}
		added, err := aud.AddMembers(ctx, g.AccountID, g.SegmentID, g.IDValues)
		if err != nil {
			// Segment may have been deleted since the signal landed — the
			// analytics store keeps the record; nothing to reconcile into.
			log.Warn("profile-builder: reconcile skipped segment", "segment", g.SegmentID, "error", err)
			continue
		}
		if added > 0 {
			res.Reconciled += added
			changed[g.SegmentID] = g.AccountID
		}
	}
	return nil
}

func reconcileProfileSignals(ctx context.Context, lake datalake.Store, aud *audiencepg.Store,
	log *slog.Logger, res *Result, changed map[string]string,
) error {
	rows, err := lake.Read(ctx, "profile_signals", datalake.Filter{})
	if err != nil {
		return fmt.Errorf("read profile_signals: %w", err)
	}
	type segKey struct{ segID, accountID string }
	ids := map[segKey][]string{}
	for _, rec := range rows {
		k := segKey{segID: str(rec["segment_id"]), accountID: str(rec["account_id"])}
		v := str(rec["id_value"])
		if k.segID == "" || k.accountID == "" || v == "" {
			continue
		}
		ids[k] = append(ids[k], v)
	}
	for k, values := range ids {
		added, err := aud.AddMembers(ctx, k.accountID, k.segID, values)
		if err != nil {
			// Segment may have been deleted since the signal landed — the lake
			// keeps the record; nothing to reconcile into.
			log.Warn("profile-builder: reconcile skipped segment", "segment", k.segID, "error", err)
			continue
		}
		if added > 0 {
			res.Reconciled += added
			changed[k.segID] = k.accountID
		}
	}
	return nil
}

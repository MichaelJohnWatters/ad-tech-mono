//go:build integration

// Parity test (ADR 0006 phase 2): the profile-builder must enroll the EXACT
// same audience whether it aggregates behaviour_signals/profile_signals in Go
// over the lake (the old path) or via server-side ClickHouse GROUP BY (the new
// path). A subtle SQL drift changes who's in an audience, so this seeds one
// identical dataset into BOTH stores, runs the builder each way against the
// same Postgres, and asserts the resulting audience_segment_members match.
//
// Requires a live Postgres (DATABASE_URL or the local default) with the
// audience/identity migrations applied, and a live ClickHouse (CLICKHOUSE_ADDR)
// with the phase-1 behaviour_signals/profile_signals tables. Skips otherwise.
//
//	CLICKHOUSE_ADDR=127.0.0.1:9010 go test -tags integration ./pkg/profilebuilder/...
//
// Coverage: behavioural rule (tag + window + min_count), site_visit
// account-scoping, category matching, lookalike, and reconcile.
package profilebuilder

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBuilderParity_LakeVsClickHouse(t *testing.T) {
	ctx := context.Background()

	// --- Postgres ---
	pgURL := os.Getenv("DATABASE_URL")
	if pgURL == "" {
		pgURL = routes.DefaultPostgresURL
	}
	db, err := sql.Open("postgres", pgURL)
	if err != nil {
		t.Skipf("postgres open: %v", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("postgres unreachable (%v) — start the local stack or set DATABASE_URL", err)
	}
	var haveSegs bool
	_ = db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'audience_segments')`).Scan(&haveSegs)
	if !haveSegs {
		t.Skip("audience_segments table missing — migrations not applied")
	}

	// --- ClickHouse ---
	chAddr := os.Getenv("CLICKHOUSE_ADDR")
	if chAddr == "" {
		t.Skip("set CLICKHOUSE_ADDR to run the phase-2 parity test")
	}
	q, err := NewCHBehaviourQuerier(CHConfig{
		Addrs:    []string{chAddr},
		Database: envOr("CLICKHOUSE_DB", "adtech"),
		Username: envOr("CLICKHOUSE_USER", "adtech"),
		Password: envOr("CLICKHOUSE_PASSWORD", "adtech-local-dev"),
	})
	if err != nil {
		t.Skipf("clickhouse connect (%v)", err)
	}
	defer q.Close()
	ch := q.db // reuse the pool for seeding

	now := time.Now().UTC()
	uniq := now.UnixNano()
	p := fmt.Sprintf("par-%d-", uniq) // run-unique id prefix
	cat := fmt.Sprintf("cat%d", uniq) // run-unique category (lookalike isolation)

	userA := p + "userA"
	deviceA2 := p + "deviceA2" // same person as userA via identity edge
	visitor := p + "visitor"   // qualifies the site_visit rule
	stranger := p + "stranger" // foreign-account site_visit — must NOT enroll
	seedU := p + "seed"        // lookalike seed person (has cat)
	twin := p + "twin"         // resembles the seed (has cat) → lookalike member
	onboard := p + "onboard"   // profile_signals reconcile target

	// --- Postgres seed: account, identity edge, segments, seed members ---
	acct := seedAccount(t, db, p)
	seedIdentityEdge(t, db, userA, deviceA2)

	// Behavioural rule: kind=click, tag=<uniq>, min_count=2, window=7d.
	tag := p + "tag"
	behSeg := seedSegment(t, db, acct, p+"beh", "behavioral",
		fmt.Sprintf(`{"event":"click","tag":%q,"min_count":2,"window_days":7}`, tag))
	// site_visit rule scoped (by the builder) to THIS account.
	visitSeg := seedSegment(t, db, acct, p+"visit", "retargeting",
		fmt.Sprintf(`{"event":"site_visit","tag":%q,"min_count":1,"window_days":30}`, tag))
	// Category rule: kind=impression AND category present.
	catSeg := seedSegment(t, db, acct, p+"cat", "behavioral",
		fmt.Sprintf(`{"event":"impression","category":%q,"min_count":1,"window_days":30}`, strings.ToUpper(cat)))
	// Lookalike over a seed segment whose one member is seedU.
	seedSeg := seedSegment(t, db, acct, p+"seed", "first_party", "")
	addMember(t, db, acct, seedSeg, seedU)
	lookSeg := seedSegment(t, db, acct, p+"look", "lookalike",
		fmt.Sprintf(`{"kind":"lookalike","seed_segment":%q,"top_categories":5,"min_similarity":0.99,"max_members":100}`, seedSeg))
	// Reconcile target segment (onboarded rows replay into it).
	recSeg := seedSegment(t, db, acct, p+"rec", "first_party", "")

	// --- Behaviour rows (identical into lake + ClickHouse) ---
	type beh struct {
		kind, user, tag, cat, acct string
	}
	rows := []beh{
		// behavioural: userA gets 2 clicks with the tag → qualifies (min 2).
		{"click", userA, tag, "", ""},
		{"click", userA, tag, "", ""},
		// deviceA2 has NO behaviour — it must enter via cluster expansion.
		// site_visit: visitor, scoped to this account → qualifies.
		{"site_visit", visitor, tag, "", acct},
		// site_visit under a FOREIGN account, same tag → must NOT enroll.
		{"site_visit", stranger, tag, "", "some-other-account"},
		// category: catUser impression carrying the (mixed-case, spaced) cat.
		{"impression", p + "catuser", "", " " + strings.Title(cat) + " ,news", ""},
		// lookalike inputs: seed + twin share the unique category.
		{"impression", seedU, "", cat, ""},
		{"impression", twin, "", cat, ""},
	}
	catUser := p + "catuser"

	lake := datalake.NewMemory(discardLog())
	seedBehaviour(t, ctx, lake, ch, now, func(add func(kind, user, tag, categories, accountID string)) {
		for _, r := range rows {
			add(r.kind, r.user, r.tag, r.cat, r.acct)
		}
	})

	// --- Profile (reconcile) rows: onboard into recSeg, in both stores ---
	seedProfile(t, ctx, lake, ch, now, acct, recSeg, []string{onboard})

	// Wait for ClickHouse parts to be visible.
	waitCount(t, ch, "behaviour_signals", "user_id LIKE '"+p+"%'", len(rows))
	waitCount(t, ch, "profile_signals", "account_id = '"+acct+"'", 1)

	segIDs := []string{behSeg, visitSeg, catSeg, lookSeg, recSeg}

	// --- Run 1: LAKE path ---
	resetMembers(t, db, segIDs)
	if _, err := Run(ctx, Config{DB: db, Lake: lake, Log: discardLog(), Now: now}); err != nil {
		t.Fatalf("lake-path Run: %v", err)
	}
	lakeMembers := snapshotMembers(t, db, segIDs, p)

	// --- Run 2: CLICKHOUSE path ---
	resetMembers(t, db, segIDs)
	// Lake still supplied for the clustering artifact WRITE; the three READS
	// go through Behaviour.
	if _, err := Run(ctx, Config{DB: db, Lake: lake, Behaviour: q, Log: discardLog(), Now: now}); err != nil {
		t.Fatalf("clickhouse-path Run: %v", err)
	}
	chMembers := snapshotMembers(t, db, segIDs, p)

	// --- Parity ---
	for _, seg := range segIDs {
		if !equalStringSets(lakeMembers[seg], chMembers[seg]) {
			t.Errorf("segment %s membership differs:\n  lake = %v\n  ch   = %v", seg, lakeMembers[seg], chMembers[seg])
		}
	}

	// Spot-check the specific semantics the parity guards (both paths, so
	// asserting either is enough — assert the ClickHouse path).
	m := chMembers
	assertHas(t, m[behSeg], userA, "behavioural: userA (2 tagged clicks ≥ min_count)")
	assertHas(t, m[behSeg], deviceA2, "behavioural: deviceA2 via cluster expansion")
	assertHas(t, m[visitSeg], visitor, "site_visit: same-account visitor")
	assertMissing(t, m[visitSeg], stranger, "site_visit: foreign-account row must NOT enroll (account scoping)")
	assertHas(t, m[catSeg], catUser, "category: mixed-case/spaced category matched")
	assertHas(t, m[lookSeg], twin, "lookalike: twin resembles the seed")
	assertMissing(t, m[lookSeg], seedU, "lookalike: seed person never self-enrolls")
	assertHas(t, m[recSeg], onboard, "reconcile: onboarded id replayed into segment")
}

// --- helpers ---

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func seedAccount(t *testing.T, db *sql.DB, p string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(
		`INSERT INTO accounts (name, email, type) VALUES ($1, $2, 'advertiser') RETURNING id::text`,
		p+"acct", p+"acct@test.local").Scan(&id); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM accounts WHERE id = $1::uuid`, id) })
	return id
}

func seedIdentityEdge(t *testing.T, db *sql.DB, a, b string) {
	t.Helper()
	if _, err := db.Exec(`
INSERT INTO identity_graph (user_id, linked_id, source, link_type, confidence, created_at)
VALUES ($1, $2, 'crm_match', 'cross_device', 1.0, now())
ON CONFLICT DO NOTHING`, a, b); err != nil {
		t.Fatalf("seed identity edge: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM identity_graph WHERE user_id = $1 OR linked_id = $1`, a) })
}

func seedSegment(t *testing.T, db *sql.DB, acct, name, typ, rule string) string {
	t.Helper()
	var id string
	var ruleArg any
	if rule != "" {
		ruleArg = rule
	}
	if err := db.QueryRow(`
INSERT INTO audience_segments (account_id, name, type, status, source, rule)
VALUES ($1::uuid, $2, $3, 'active', 'profile_builder', $4::jsonb)
RETURNING id::text`, acct, name, typ, ruleArg).Scan(&id); err != nil {
		t.Fatalf("seed segment %s: %v", name, err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM audience_segments WHERE id = $1::uuid`, id) })
	return id
}

func addMember(t *testing.T, db *sql.DB, acct, seg, uid string) {
	t.Helper()
	aud := audiencepg.New(db)
	if _, err := aud.AddMembers(context.Background(), acct, seg, []string{uid}, "test-seed", ""); err != nil {
		t.Fatalf("seed member: %v", err)
	}
}

// seedBehaviour writes identical behaviour_signals rows into the memory lake
// and ClickHouse via the supplied emit callback.
func seedBehaviour(t *testing.T, ctx context.Context, lake datalake.Store, ch *sql.DB, now time.Time,
	emit func(add func(kind, user, tag, categories, accountID string)),
) {
	t.Helper()
	var lakeRecs []datalake.Record
	add := func(kind, user, tag, categories, accountID string) {
		lakeRecs = append(lakeRecs, datalake.Record{
			"kind": kind, "user_id": user, "household_id": "", "tag": tag,
			"categories": categories, "account_id": accountID, "observed_at": now,
		})
		if _, err := ch.ExecContext(ctx,
			`INSERT INTO behaviour_signals (kind, user_id, tag, categories, account_id, observed_at) VALUES (?,?,?,?,?,?)`,
			kind, user, tag, categories, accountID, now); err != nil {
			t.Fatalf("ch behaviour insert: %v", err)
		}
	}
	emit(add)
	schema := datalake.Schema{Version: 1, Columns: []datalake.Column{
		{Name: "kind", Type: "string"}, {Name: "user_id", Type: "string"},
		{Name: "household_id", Type: "string"}, {Name: "tag", Type: "string"},
		{Name: "categories", Type: "string"}, {Name: "account_id", Type: "string"},
		{Name: "observed_at", Type: "timestamp"},
	}}
	if err := lake.Write(ctx, "behaviour_signals", lakeRecs, schema); err != nil {
		t.Fatalf("lake behaviour write: %v", err)
	}
}

func seedProfile(t *testing.T, ctx context.Context, lake datalake.Store, ch *sql.DB, now time.Time,
	acct, seg string, idValues []string,
) {
	t.Helper()
	var recs []datalake.Record
	for _, v := range idValues {
		recs = append(recs, datalake.Record{
			"account_id": acct, "segment_id": seg, "id_value": v, "id_type": "hashed_email", "observed_at": now,
		})
		if _, err := ch.ExecContext(ctx,
			`INSERT INTO profile_signals (account_id, segment_id, id_value, id_type, observed_at) VALUES (?,?,?,?,?)`,
			acct, seg, v, "hashed_email", now); err != nil {
			t.Fatalf("ch profile insert: %v", err)
		}
	}
	schema := datalake.Schema{Version: 1, Columns: []datalake.Column{
		{Name: "account_id", Type: "string"}, {Name: "segment_id", Type: "string"},
		{Name: "id_value", Type: "string"}, {Name: "id_type", Type: "string"},
		{Name: "observed_at", Type: "timestamp"},
	}}
	if err := lake.Write(ctx, "profile_signals", recs, schema); err != nil {
		t.Fatalf("lake profile write: %v", err)
	}
}

func waitCount(t *testing.T, ch *sql.DB, table, where string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n uint64
		if err := ch.QueryRow(`SELECT count() FROM ` + table + ` WHERE ` + where).Scan(&n); err != nil {
			t.Fatalf("ch count %s: %v", table, err)
		}
		if int(n) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ClickHouse %s rows not visible: got %d want %d", table, n, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func resetMembers(t *testing.T, db *sql.DB, segs []string) {
	t.Helper()
	for _, s := range segs {
		if _, err := db.Exec(`DELETE FROM audience_segment_members WHERE segment_id = $1::uuid`, s); err != nil {
			t.Fatalf("reset members: %v", err)
		}
	}
}

// snapshotMembers returns per-segment member id sets, restricted to this run's
// id prefix so cross-run rows in the shared ClickHouse can't skew the compare.
func snapshotMembers(t *testing.T, db *sql.DB, segs []string, prefix string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, s := range segs {
		rows, err := db.Query(
			`SELECT user_id FROM audience_segment_members WHERE segment_id = $1::uuid AND user_id LIKE $2`,
			s, prefix+"%")
		if err != nil {
			t.Fatalf("snapshot members: %v", err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				t.Fatalf("scan member: %v", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		sort.Strings(ids)
		out[s] = ids
	}
	return out
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertHas(t *testing.T, set []string, want, msg string) {
	t.Helper()
	for _, s := range set {
		if s == want {
			return
		}
	}
	t.Errorf("%s — %q not in %v", msg, want, set)
}

func assertMissing(t *testing.T, set []string, notWant, msg string) {
	t.Helper()
	for _, s := range set {
		if s == notWant {
			t.Errorf("%s — %q unexpectedly in %v", msg, notWant, set)
		}
	}
}

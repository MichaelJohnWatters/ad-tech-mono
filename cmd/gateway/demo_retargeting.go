package main

// demo_retargeting.go — the staff-only guided "Retargeting" demo.
//
// It teaches, step by step for ONE synthetic person, how VISITING an
// advertiser's page (the retargeting pixel) turns that user into a member of a
// retargeting audience that then gets targeted. This is the behavioural/lake
// sibling of the onboarding demo (demo_onboarding.go): where onboarding uploads
// a list (a plain, PG-only segment), retargeting fires a page VISIT that lands
// a consent-gated `site_visit` row in the behaviour_signals Delta lake, which a
// behavioural RULE (event=site_visit, tag=pricing-page) then matches — enrolling
// and cross-device-expanding the person.
//
// The REAL flow this makes concrete:
//
//	/v1/t/rt pixel (visit) → tracker publishes a consent-gated site_visit row →
//	it lands in the behaviour_signals lake → the profile-builder's behavioural-
//	rule job reads behaviour_signals, matches the rule (event + tag) → enrols the
//	person → cluster-expands membership to every id in the cluster → the user is
//	now in the retargeting segment, targetable in the next auction.
//
// The 5 steps the staff portal renders:
//
//  1. SETUP  — a synthetic person with four linked ids (hashed email, cookie,
//              mobile ad id, household) + a retargeting RULE segment
//              "Retargeting: pricing visitors" (event=site_visit, tag=
//              pricing-page). Show the rule.
//  2. VISIT  — fire the REAL /v1/t/rt pixel for the demo user visiting the
//              pricing page (show it 200s) AND write the site_visit
//              behaviour_signals row to the lake. Show the normalised row.
//  3. BEFORE — the profile view for the demo user: NOT yet in the retargeting
//              segment (the rule hasn't run).
//  4. MATCH  — run the REAL lake-backed profile-builder: its behavioural-rule
//              job reads behaviour_signals, the rule matches the site_visit row,
//              enrols the person and cluster-expands to every id.
//  5. AFTER  — the profile view again: the demo user AND their cluster (cookie,
//              ifa) are now in "Retargeting: pricing visitors". The household id
//              is intentionally EXCLUDED from clustering (a household groups the
//              people behind one IP, it does not identify one person).
//
// Staff-only: GET (support:read) returns the current/last-run state; POST /run
// (support:update) resets + runs synchronously.
//
// The pixel-fire + lake-write + builder run live behind the retargetingBackend
// interface so unit tests use a fake (no live tracker / lake / builder). The
// live backend fires the genuine /v1/t/rt entry point, writes the exact
// site_visit behaviour_signals row directly to the same lake ObjectStore the
// pipeline uses (deterministic — no waiting on the ~15s pipeline flush), and
// runs the real pkg/profilebuilder WITH a Lake so its behavioural rule fires.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/lib/pq"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/profilebuilder"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
	pgstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// Fixed, human-legible id values so the story reads the same every run. A
// distinct "demo-rt-alice" set (not the onboarding demo's "demo-alice") so the
// two demos never fight over the same edges/segments/clusters. The hh: id is
// the household grouping the clusterer deliberately won't merge through.
const (
	rtEmailID     = "sha256:demo-rt-alice"
	rtCookieID    = "cookie:demo-rt-alice"
	rtIfaID       = "ifa:demo-rt-alice"
	rtHouseholdID = "hh:demo-rt-alice"

	rtSegmentName = "Retargeting: pricing visitors"
	rtAccountName = "Retargeting Demo (synthetic)"

	// rtTag is the advertiser's self-chosen pixel tag; the rule filters
	// site_visit rows to exactly this tag (see profilebuilder.Rule.Tag).
	rtTag = "pricing-page"
)

// rtAccountID isolates all demo data behind a stable derived account id, so a
// run never touches a real tenant. Distinct from the onboarding demo's account.
func rtAccountID() string { return idgen.Derive("account", "demo-retargeting") }

func rtAllIDs() []string { return []string{rtEmailID, rtCookieID, rtIfaID, rtHouseholdID} }

// rtRuleJSON is the behavioural retargeting rule persisted on the segment's
// rule JSONB: enroll every person with a site_visit behaviour row carrying this
// tag in the window. accountID is NOT in the JSON — the builder stamps it from
// the segment's own account so pixel tags can't collide across tenants.
func rtRuleJSON() string {
	r := profilebuilder.Rule{Event: "site_visit", Tag: rtTag, MinCount: 1, WindowDays: 30}
	b, _ := json.Marshal(r)
	return string(b)
}

// rtVisitRow is the normalised behaviour_signals-shaped row shown in the VISIT
// step (a subset of the lake columns — the ones that matter for the story).
type rtVisitRow struct {
	Kind       string `json:"kind"`
	UserID     string `json:"user_id"`
	Tag        string `json:"tag"`
	AccountID  string `json:"account_id"`
	Consent    bool   `json:"consent"`
	ObservedAt string `json:"observed_at"`
}

// rtRuleView is the rule shown in the SETUP step.
type rtRuleView struct {
	SegmentName string `json:"segment_name"`
	Event       string `json:"event"`
	Tag         string `json:"tag"`
	WindowDays  int    `json:"window_days"`
	Rule        string `json:"rule"` // the raw JSONB
}

// rtVisitResult reports the VISIT step: the real pixel fire + the lake write.
type rtVisitResult struct {
	PixelURL    string     `json:"pixel_url"`
	PixelStatus int        `json:"pixel_status"` // 200 = the genuine entry point 200s
	PixelFired  bool       `json:"pixel_fired"`  // false when the tracker was unreachable
	Row         rtVisitRow `json:"row"`
}

// rtMatchResult is the profile-builder outcome for the MATCH step.
type rtMatchResult struct {
	Clusters       int      `json:"clusters"`
	ClusterMembers int      `json:"cluster_members"`
	RuleSegments   int      `json:"rule_segments"`
	Enrolled       int      `json:"enrolled"` // memberships added by rule evaluation (post-expansion)
	MembersBefore  []string `json:"members_before"`
	MembersAfter   []string `json:"members_after"`
	Added          []string `json:"added"`
	Excluded       string   `json:"excluded"`
}

// retargetingBackend is the live-machinery boundary the orchestrator drives:
// firing the real pixel, writing the site_visit lake row, and running the real
// lake-backed profile-builder. A fake implements it in unit tests so no live
// tracker/lake/builder is needed there; the production impl
// (httpLakeRetargetingBackend) mocks nothing.
type retargetingBackend interface {
	// FirePixel fires the genuine /v1/t/rt pixel for the demo visit and
	// returns its HTTP status (proving the real entry point 200s). A tracker
	// outage is non-fatal — the demo relies on WriteVisitRow for the builder
	// input, not on the async pixel→NATS→pipeline→lake path.
	FirePixel(ctx context.Context, row rtVisitRow) (status int, fired bool, pixelURL string)
	// WriteVisitRow writes exactly one site_visit behaviour_signals row to the
	// lake — the deterministic builder input (no waiting on the ~15s pipeline
	// flush a real pixel would take to land).
	WriteVisitRow(ctx context.Context, row rtVisitRow) error
	// RunBuilder runs the REAL profile-builder WITH a Lake, so its behavioural-
	// rule job reads behaviour_signals and the retargeting rule fires.
	RunBuilder(ctx context.Context) (profilebuilder.Result, error)
}

// rtDemoOrchestrator wires the demo to its stores + the live backend.
type rtDemoOrchestrator struct {
	db       *sql.DB
	aud      *audiencepg.Store
	resolver identityResolver
	backend  retargetingBackend
	log      *slog.Logger
}

// resetDemo makes the demo repeatable and deterministic: it wipes the demo
// account's segments/members + the synthetic edges/clusters and re-seeds the
// four edges + the site_visit RULE segment. Everything is keyed on the isolated
// demo account so it can never touch a real tenant. Lake behaviour rows may
// accumulate across runs — that's idempotent: the rule re-matches the same demo
// user's rows and enrollment is recomputed wholesale each run.
func (o *rtDemoOrchestrator) resetDemo(ctx context.Context) error {
	acct := rtAccountID()

	// Idempotent account row so the demo's segment has a valid FK target.
	if _, err := o.db.ExecContext(ctx, `
INSERT INTO accounts (id, name, email, type, status, created_at, updated_at)
VALUES ($1, $2, $3, 'advertiser', 'active', now(), now())
ON CONFLICT (id) DO NOTHING`,
		acct, rtAccountName, "demo-retargeting@synthetic.local"); err != nil {
		return fmt.Errorf("upsert demo account: %w", err)
	}

	// Wipe prior segments + their members (members cascade via segment_id).
	if _, err := o.db.ExecContext(ctx,
		`DELETE FROM audience_segment_members WHERE account_id = $1::uuid`, acct); err != nil {
		return fmt.Errorf("wipe demo members: %w", err)
	}
	if _, err := o.db.ExecContext(ctx,
		`DELETE FROM audience_segments WHERE account_id = $1::uuid`, acct); err != nil {
		return fmt.Errorf("wipe demo segments: %w", err)
	}

	// Wipe the four synthetic edges (either direction) + any cluster rows that
	// mention the demo ids, so a prior run's materialised cluster can't leak in.
	ids := rtAllIDs()
	if _, err := o.db.ExecContext(ctx,
		`DELETE FROM identity_graph WHERE user_id = ANY($1) OR linked_id = ANY($1)`,
		pq.Array(ids)); err != nil {
		return fmt.Errorf("wipe demo edges: %w", err)
	}
	if _, err := o.db.ExecContext(ctx,
		`DELETE FROM identity_clusters WHERE member_id = ANY($1)`, pq.Array(ids)); err != nil {
		return fmt.Errorf("wipe demo clusters: %w", err)
	}

	// Re-seed the four edges. All hang off the email so the person is one
	// connected component; confidence ≥ MinConfidence (0.5) so they cluster.
	// The household edge is co-seeded but the clusterer excludes hh: links.
	store := pgstore.NewFromDB(o.db)
	edges := []pgstore.IdentityEdge{
		{UserID: rtEmailID, LinkedID: rtCookieID, Source: identity.SourceHashedEmail, LinkType: identity.LinkCrossDevice, Confidence: 1.0},
		{UserID: rtEmailID, LinkedID: rtIfaID, Source: identity.SourceDeviceID, LinkType: identity.LinkCrossDevice, Confidence: 1.0},
		{UserID: rtEmailID, LinkedID: rtHouseholdID, Source: identity.SourceHousehold, LinkType: identity.LinkHousehold, Confidence: identity.HouseholdConfidence},
	}
	if _, err := store.LinkIdentity(ctx, edges); err != nil {
		return fmt.Errorf("seed demo edges: %w", err)
	}

	// (Re)create the RULE segment through the real audience store, then set its
	// rule JSONB — the audience store has no rule-writing method, so the demo
	// stamps the behavioural rule directly (parameterised) on the segment it
	// just created. This is the segment the builder's behavioural-rule job
	// evaluates.
	segID, err := o.aud.UpsertSegment(ctx, acct, rtSegmentName, "first_party", "demo_retargeting", "dsp_private")
	if err != nil {
		return fmt.Errorf("create demo rule segment %q: %w", rtSegmentName, err)
	}
	if _, err := o.db.ExecContext(ctx,
		`UPDATE audience_segments SET rule = $3::jsonb, updated_at = now()
		 WHERE id = $1 AND account_id = $2::uuid`, segID, acct, rtRuleJSON()); err != nil {
		return fmt.Errorf("set demo rule: %w", err)
	}
	return nil
}

// rtSegmentID is the deterministic id of the demo rule segment.
func rtSegmentID() string { return idgen.Derive("segment", rtAccountID()+"/"+rtSegmentName) }

// run executes the reset then the 5 steps synchronously and returns the
// assembled timeline.
func (o *rtDemoOrchestrator) run(ctx context.Context) (demoResponse, error) {
	acct := rtAccountID()
	if err := o.resetDemo(ctx); err != nil {
		return demoResponse{}, err
	}
	segID := rtSegmentID()

	// --- The visit: fire the REAL pixel + write the lake row. ---
	// The row is EXACTLY what a real /v1/t/rt visit's behaviour row looks like:
	// the demo user's id, kind=site_visit, the pixel tag, the demo account,
	// consent granted, observed now. A real pixel reaches the lake via pixel→
	// NATS→pipeline→a ~15s Parquet flush — too flaky to wait on in a demo, so we
	// place the row in the lake now to watch the rule fire immediately, while
	// ALSO firing the genuine pixel to show the real entry point + 200.
	now := time.Now().UTC()
	row := rtVisitRow{
		Kind: "site_visit", UserID: rtEmailID, Tag: rtTag, AccountID: acct,
		Consent: true, ObservedAt: now.Format(time.RFC3339),
	}
	status, fired, pixelURL := o.backend.FirePixel(ctx, row)
	if err := o.backend.WriteVisitRow(ctx, row); err != nil {
		return demoResponse{}, fmt.Errorf("demo write visit row: %w", err)
	}
	visit := rtVisitResult{PixelURL: pixelURL, PixelStatus: status, PixelFired: fired, Row: row}

	// --- BEFORE — profile view; NOT yet in the retargeting segment. ---
	before, err := buildProfileView(ctx, o.db, o.resolver, rtEmailID)
	if err != nil {
		return demoResponse{}, fmt.Errorf("demo before-view: %w", err)
	}
	beforeMembers := rtSegmentMembers(before, segID)

	// --- MATCH & EXPAND — run the REAL lake-backed profile-builder. ---
	res, err := o.backend.RunBuilder(ctx)
	if err != nil {
		return demoResponse{}, fmt.Errorf("demo profile-builder: %w", err)
	}

	// --- AFTER — profile view again; the diff. ---
	after, err := buildProfileView(ctx, o.db, o.resolver, rtEmailID)
	if err != nil {
		return demoResponse{}, fmt.Errorf("demo after-view: %w", err)
	}
	afterMembers := rtSegmentMembers(after, segID)

	// Assemble the 5-step timeline from the computed pieces (pure, DB-free —
	// see the unit test), then attach the full profile views to the BEFORE/AFTER
	// steps (those need the DB read, so they can't live in the pure assembler).
	steps := assembleRetargetingSteps(beforeMembers, afterMembers, res, visit)
	steps[2].Data.(map[string]interface{})["profile"] = before
	steps[4].Data.(map[string]interface{})["profile"] = after

	ranAt := time.Now().UTC()
	return demoResponse{
		Ran: true, AccountID: acct, Steps: steps, RanAt: &ranAt,
		Summary: "visit → behaviour row (lake) → rule → segment → expanded → targetable: one page VISIT fires the retargeting pixel, which lands a consent-gated site_visit row in the behaviour_signals lake; the profile-builder's behavioural rule matches it, enrols the person, and cluster-expands the membership across their cookie and mobile ad id (but not the household) — turning a single visit into a cross-device retargeting audience.",
	}, nil
}

// assembleRetargetingSteps builds the 5-step timeline from the computed member
// sets, the profile-builder result and the visit outcome. Pure (no DB / no
// backend) so the UI contract — 5 numbered steps, AFTER ⊇ BEFORE, the diff is
// exactly what the rule added, honest narration when nothing enrolled — is
// unit-testable. run() attaches the full profile views to steps 3 + 5 after.
func assembleRetargetingSteps(beforeMembers, afterMembers []string, res profilebuilder.Result, visit rtVisitResult) []demoStep {
	added := demoDiff(beforeMembers, afterMembers)
	edges := []demoEdge{
		{From: rtEmailID, To: rtCookieID, Source: identity.SourceHashedEmail, LinkType: identity.LinkCrossDevice, Confidence: 1.0},
		{From: rtEmailID, To: rtIfaID, Source: identity.SourceDeviceID, LinkType: identity.LinkCrossDevice, Confidence: 1.0},
		{From: rtEmailID, To: rtHouseholdID, Source: identity.SourceHousehold, LinkType: identity.LinkHousehold, Confidence: identity.HouseholdConfidence, Household: true},
	}
	match := rtMatchResult{
		Clusters: res.Clusters, ClusterMembers: res.ClusterMembers, RuleSegments: res.RuleSegments,
		Enrolled: res.Enrolled, MembersBefore: beforeMembers, MembersAfter: afterMembers, Added: added,
		Excluded: rtHouseholdID + " (household) — excluded from clustering by design: a household groups the people behind one IP, it does not identify one person, so it never becomes a retargeting-segment member.",
	}
	return []demoStep{
		{
			N: 1, Title: "Setup — a person, and a retargeting rule",
			Narration: "The identity graph already links this person's hashed email, browser cookie, mobile ad id (ifa) and household (hh:). An advertiser has a RULE segment \"Retargeting: pricing visitors\" — enroll anyone who fires a site_visit pixel tagged \"pricing-page\". No members yet; a visit is what fills it.",
			Data: map[string]interface{}{
				"edges": edges,
				"rule": rtRuleView{
					SegmentName: rtSegmentName, Event: "site_visit", Tag: rtTag,
					WindowDays: 30, Rule: rtRuleJSON(),
				},
			},
		},
		{
			N: 2, Title: "The visit — the retargeting pixel fires",
			Narration: "The user visits the advertiser's pricing page; its /v1/t/rt pixel fires with the user id + tag \"pricing-page\". Consent is evaluated at the pixel (no consent = no row = no retargeting). A real pixel reaches the lake via pixel→NATS→pipeline→a ~15s flush; we place the SAME behaviour_signals row in the lake now so the rule can fire immediately — the pixel above still fires for real to prove the entry point.",
			Data:      map[string]interface{}{"visit": visit},
		},
		{
			N: 3, Title: "Before — not in the segment yet",
			Narration: "Looking up the user in the profile store: the \"Retargeting: pricing visitors\" segment has NO members. The visit landed a behaviour row, but the rule hasn't run — so the user isn't retargetable yet.",
			Data:      map[string]interface{}{"segment_members": beforeMembers},
		},
		{
			N: 4, Title: "Match & expand — the rule fires",
			Narration: "The batch profile-builder runs for real WITH the lake: its behavioural-rule job reads behaviour_signals, the site_visit row matches the rule (event + tag), the person is enrolled, and enrollment cluster-EXPANDS to every id in the cluster. The household id is left out on purpose. This is the exact production code, run lake-backed.",
			Data:      match,
		},
		{
			N: 5, Title: "After — retargetable across the cluster",
			Narration: retargetingAfterNarration(afterMembers, added),
			Data:      map[string]interface{}{"segment_members": afterMembers, "added": added},
		},
	}
}

// retargetingAfterNarration reports the AFTER step honestly: when the rule fired
// it lists the expanded membership + what expansion added; when nothing enrolled
// (tag mismatch / no consent) it plainly says 0 ids rather than claiming a win.
func retargetingAfterNarration(afterMembers, added []string) string {
	if len(afterMembers) == 0 {
		return "The \"Retargeting: pricing visitors\" segment contains 0 ids: the visit's behaviour row did not match the rule (a tag mismatch, or consent was withheld so no row was ever written). No enrolment, nothing to expand — the user is not retargetable."
	}
	return fmt.Sprintf("The \"Retargeting: pricing visitors\" segment now contains %d ids: %v. The visit enrolled the email and expansion added %v via the identity cluster. The household id (%s) stays out. The user is now targetable in the next auction.", len(afterMembers), afterMembers, added, rtHouseholdID)
}

// rtSegmentMembers pulls the sorted set of demo-segment member ids out of a
// profile view (reuses the shared profileView shape from buildProfileView).
func rtSegmentMembers(v profileView, segmentID string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range v.Memberships {
		if m.SegmentID == segmentID && !seen[m.MemberID] {
			seen[m.MemberID] = true
			out = append(out, m.MemberID)
		}
	}
	sort.Strings(out)
	return out
}

// demoRetargetingHandler serves both endpoints:
//
//	GET  /v1/api/demo/retargeting      (support:read)   — current/last-run state
//	POST /v1/api/demo/retargeting/run  (support:update) — reset + run, 5 steps
//
// Mirrors demoOnboardingHandler exactly: permission-gate BEFORE the store check
// so a non-staff caller always gets 403 (never a 503 that would leak wiring).
func demoRetargetingHandler(o *rtDemoOrchestrator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "support:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
		case http.MethodPost:
			if !can(claims, "support:update") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		if o == nil || o.db == nil || o.backend == nil {
			http.Error(w, `{"error":"demo unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		switch r.Method {
		case http.MethodGet:
			resp, err := o.currentState(r.Context())
			if err != nil {
				o.log.Error("demo retargeting state failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(resp)

		case http.MethodPost:
			resp, err := o.run(r.Context())
			if err != nil {
				o.log.Error("demo retargeting run failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			o.log.Info("demo retargeting run", "account", resp.AccountID, "steps", len(resp.Steps), "actor", claims.UserID)
			_ = json.NewEncoder(w).Encode(resp)
		}
	}
}

// currentState answers the GET: does the demo rule segment have members yet?
// Zero members means "not run" (or the rule hasn't matched). Mirrors the
// onboarding demo's persisted-state posture.
func (o *rtDemoOrchestrator) currentState(ctx context.Context) (demoResponse, error) {
	acct := rtAccountID()
	segID := rtSegmentID()
	var members int
	err := o.db.QueryRowContext(ctx,
		`SELECT count(*) FROM audience_segment_members WHERE segment_id = $1::uuid`, segID).Scan(&members)
	if err != nil && err != sql.ErrNoRows {
		return demoResponse{}, err
	}
	if members == 0 {
		return demoResponse{Ran: false, AccountID: acct,
			Summary: "The demo has not been run yet. Click \"Run demo\" to watch one synthetic user's page VISIT (the retargeting pixel) turn into an expanded, cross-device retargeting audience."}, nil
	}
	view, err := buildProfileView(ctx, o.db, o.resolver, rtEmailID)
	if err != nil {
		return demoResponse{}, err
	}
	seg := rtSegmentMembers(view, segID)
	return demoResponse{
		Ran: true, AccountID: acct,
		Steps: []demoStep{{
			N: 5, Title: "Last run — retargetable across the cluster",
			Narration: fmt.Sprintf("The \"Retargeting: pricing visitors\" segment currently has %d members: %v. Run the demo again to replay the full 5-step story.", len(seg), seg),
			Data:      map[string]interface{}{"profile": view, "segment_members": seg},
		}},
		Summary: "visit → behaviour row (lake) → rule → segment → expanded → targetable. Run the demo to replay all five steps.",
	}, nil
}

// --- live backend: real pixel + real lake write + real lake-backed builder ---

// behaviourSignalsLakeTable is the Delta table the pipeline writes site_visit
// rows to and the profile-builder reads for behavioural rules. Kept in sync
// with cmd/pipeline's behaviourSignalsTable (an internal const there).
const behaviourSignalsLakeTable = "behaviour_signals"

// behaviourSignalsLakeSchema mirrors cmd/pipeline's behaviour_signals sink
// schema exactly — same columns, types and partition column — so a row this
// demo writes is indistinguishable from one the pipeline would have written
// from a real pixel, and the builder reads it identically.
var behaviourSignalsLakeSchema = datalake.Schema{Version: 1, PartitionBy: "observed_at", Columns: []datalake.Column{
	{Name: "trace_id", Type: "string", Nullable: true},
	{Name: "kind", Type: "string", Nullable: true},
	{Name: "user_id", Type: "string", Nullable: true},
	{Name: "household_id", Type: "string", Nullable: true},
	{Name: "placement_id", Type: "string", Nullable: true},
	{Name: "publisher_id", Type: "string", Nullable: true},
	{Name: "campaign_id", Type: "string", Nullable: true},
	{Name: "creative_id", Type: "string", Nullable: true},
	{Name: "channel", Type: "string", Nullable: true},
	{Name: "categories", Type: "string", Nullable: true},
	{Name: "geo", Type: "string", Nullable: true},
	{Name: "device", Type: "string", Nullable: true},
	{Name: "account_id", Type: "string", Nullable: true},
	{Name: "tag", Type: "string", Nullable: true},
	{Name: "observed_at", Type: "timestamp", Nullable: false},
}}

// httpLakeRetargetingBackend is the production backend: it fires the genuine
// /v1/t/rt pixel over HTTP, writes the site_visit row straight to the lake
// ObjectStore, and runs the real pkg/profilebuilder WITH that lake.
type httpLakeRetargetingBackend struct {
	client     *http.Client
	trackerURL string         // server-reachable tracker base for the pixel fire
	db         *sql.DB        // the builder's Postgres
	lake       datalake.Store // the lake ObjectStore (behaviour_signals + clusters)
	bus        events.EventBus
	log        *slog.Logger
}

// FirePixel fires the real /v1/t/rt pixel with the demo visit's params (uid,
// aid, tag + a trace id). No consent-suppressing params are set, so the tracker
// evaluates consent as granted and would publish its own site_visit row. A
// tracker outage is non-fatal — the demo's deterministic builder input is the
// direct WriteVisitRow, not this async path.
func (b *httpLakeRetargetingBackend) FirePixel(ctx context.Context, row rtVisitRow) (int, bool, string) {
	q := url.Values{}
	q.Set("uid", row.UserID)
	q.Set("aid", row.AccountID)
	q.Set("tag", row.Tag)
	q.Set("tid", "demo-rt-"+row.ObservedAt)
	pixelURL := b.trackerURL + routes.TrackerRetarget + "?" + q.Encode()
	// No tracker configured (or no client) — skip the fire; the deterministic
	// builder input is the direct lake write, not this async path.
	if b.trackerURL == "" || b.client == nil {
		return 0, false, pixelURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pixelURL, nil)
	if err != nil {
		b.log.Warn("demo retargeting: build pixel request failed", "error", err)
		return 0, false, pixelURL
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.log.Warn("demo retargeting: pixel fire failed (non-fatal — lake write is the builder input)", "error", err)
		return 0, false, pixelURL
	}
	defer resp.Body.Close()
	return resp.StatusCode, true, pixelURL
}

// WriteVisitRow writes exactly one site_visit behaviour_signals row to the lake
// — the same table + schema the pipeline sink uses, so the builder reads it
// identically. observed_at is a real time.Time so the timestamp column + the
// event_date partition are correct.
func (b *httpLakeRetargetingBackend) WriteVisitRow(ctx context.Context, row rtVisitRow) error {
	observed, err := time.Parse(time.RFC3339, row.ObservedAt)
	if err != nil {
		observed = time.Now().UTC()
	}
	rec := datalake.Record{
		"trace_id": "demo-rt-" + row.ObservedAt, "kind": row.Kind, "user_id": row.UserID,
		"household_id": "", "placement_id": "", "publisher_id": "", "campaign_id": "",
		"creative_id": "", "channel": "", "categories": "", "geo": "", "device": "",
		"account_id": row.AccountID, "tag": row.Tag, "observed_at": observed.UTC(),
	}
	return b.lake.Write(ctx, behaviourSignalsLakeTable, []datalake.Record{rec}, behaviourSignalsLakeSchema)
}

// RunBuilder runs the real profile-builder WITH the lake so its behavioural-
// rule job fires (Lake set = behaviour_signals read + rule evaluation, unlike
// the onboarding demo's Lake:nil clustering-only run).
func (b *httpLakeRetargetingBackend) RunBuilder(ctx context.Context) (profilebuilder.Result, error) {
	return profilebuilder.Run(ctx, profilebuilder.Config{DB: b.db, Lake: b.lake, Bus: b.bus, Log: b.log})
}

// connectDatalake builds the lake reader/writer the retargeting demo needs in
// the gateway, WITHOUT the duckdb build tag: datalake.NewObjectStore over a
// pure-Go Apache-Arrow Parquet object store (Minio/S3 when s3.endpoint is set,
// else a local filesystem fallback) — exactly how cmd/pipeline + cmd/profile-
// builder construct it, reading the same keys.S3.* config and the same
// datalake bucket. Returns nil (demo 503s) if no store can be built. The bucket
// is EnsureBucket-ed here because the gateway isn't the pipeline (which does it
// at boot) — the demo may be the first writer to touch it.
func connectDatalake(ctx context.Context, cfg *config.Config, log *slog.Logger) datalake.Store {
	const fsRoot = "/tmp/adtech-datalake"
	bucket := keys.ProfileBuilder.DatalakeBucket.Get(cfg)
	if endpoint := cfg.Get(keys.S3.Endpoint.Key(), ""); endpoint != "" {
		obj, err := objs3.New(objs3.Config{
			Endpoint:  endpoint,
			AccessKey: keys.S3.AccessKey.Get(cfg),
			SecretKey: keys.S3.SecretKey.Get(cfg),
			Region:    keys.S3.Region.Get(cfg),
			UseSSL:    keys.S3.UseSSL.Get(cfg),
		})
		if err != nil {
			log.Error("demo retargeting: s3 connect failed — retargeting demo unavailable", "error", err)
			return nil
		}
		if err := obj.EnsureBucket(ctx, bucket); err != nil {
			log.Error("demo retargeting: ensure datalake bucket failed", "bucket", bucket, "error", err)
			return nil
		}
		return datalake.NewObjectStore(obj, bucket, log)
	}
	obj, err := fs.New(fsRoot)
	if err != nil {
		log.Error("demo retargeting: datalake fs store init failed — retargeting demo unavailable", "error", err)
		return nil
	}
	log.Warn("demo retargeting: s3.endpoint not set — lake uses local filesystem", "root", fsRoot)
	return datalake.NewObjectStore(obj, bucket, log)
}

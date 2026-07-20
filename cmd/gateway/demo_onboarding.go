package main

// demo_onboarding.go — the staff-only guided "Onboarding & Expansion" demo.
//
// It teaches, step by step for ONE synthetic person, how an uploaded list of a
// single hashed email becomes an expanded, cross-device targetable audience.
// Every run RESETs an isolated synthetic demo account (its own derived
// account_id, never a real tenant) so the story is deterministic and repeatable,
// then runs the REAL machinery — the real audience store for the upload, the
// real pkg/profilebuilder for the expansion — so nothing about the flow is
// mocked. The response is the 5-step timeline the staff portal renders:
//
//  1. SETUP    — a synthetic person with four linked ids in the identity graph
//                (hashed email, cookie, mobile ad id, household). Show the edges.
//  2. UPLOAD   — an advertiser uploads a 1-row list of ONLY the hashed email as a
//                plain "Demo Newsletter" segment. Show it normalised + the member.
//  3. BEFORE   — the profile view for the email: segment has exactly ONE member;
//                the cluster is already known (email+cookie+ifa+household linked).
//  4. EXPAND   — run the real profile-builder (Lake:nil → clustering + plain
//                segment expansion, the demo's exact path).
//  5. AFTER    — the profile view again: the segment now contains email+cookie+ifa.
//                The household (hh:) id is intentionally EXCLUDED from clustering
//                (a real guard: a household groups the people behind one IP, it
//                doesn't identify one person) — shown in the edges, absent from
//                the segment, and called out.
//
// Staff-only: GET (support:read) returns the current/last-run state; POST /run
// (support:update) resets + runs synchronously.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/lib/pq"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/identity"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/profilebuilder"
	pgstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// Fixed, human-legible id values so the story reads the same every run. The
// four ids belong to one synthetic person ("alice"); the hh: id is the
// household grouping the clusterer deliberately won't merge through.
const (
	demoEmailID     = "sha256:demo-alice"
	demoCookieID    = "cookie:demo-alice"
	demoIfaID       = "ifa:demo-alice"
	demoHouseholdID = "hh:demo-alice"

	demoSegmentName = "Demo Newsletter"
	demoAccountName = "Onboarding Demo (synthetic)"
)

// demoAccountID isolates all demo data behind a stable derived account id, so a
// run never touches a real tenant. Derived like every other friendly-key id.
func demoAccountID() string { return idgen.Derive("account", "demo-onboarding") }

// demoStep is one timeline step returned to the UI. Data is a shape the step's
// renderer understands (edges, upload row, profile view, expansion result).
type demoStep struct {
	N         int         `json:"n"`
	Title     string      `json:"title"`
	Narration string      `json:"narration"`
	Data      interface{} `json:"data"`
}

// demoResponse is the whole timeline plus a plain-English wrap-up.
type demoResponse struct {
	Ran       bool       `json:"ran"`
	AccountID string     `json:"account_id"`
	Steps     []demoStep `json:"steps"`
	Summary   string     `json:"summary"`
	RanAt     *time.Time `json:"ran_at,omitempty"`
}

// demoEdge is one seeded identity-graph edge, for the SETUP step.
type demoEdge struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	Source     string  `json:"source"`
	LinkType   string  `json:"link_type"`
	Confidence float64 `json:"confidence"`
	Household  bool    `json:"household"` // true = excluded from clustering
}

// demoUploadRow is the normalised profile_signals-shaped row for the UPLOAD step.
type demoUploadRow struct {
	SegmentName string `json:"segment_name"`
	IDType      string `json:"id_type"`
	IDValue     string `json:"id_value"`
	Access      string `json:"access"`
	Consent     bool   `json:"consent"`
}

// demoExpandResult is the profile-builder outcome for the EXPAND step.
type demoExpandResult struct {
	Clusters       int      `json:"clusters"`
	ClusterMembers int      `json:"cluster_members"`
	Expanded       int      `json:"expanded"` // memberships added to plain segments
	MembersBefore  []string `json:"members_before"`
	MembersAfter   []string `json:"members_after"`
	Added          []string `json:"added"`    // AFTER − BEFORE
	Excluded       string   `json:"excluded"` // the hh: id, and why
}

// demoOrchestrator wires the demo to its stores. Bus is optional (nil skips the
// profile-builder's cache invalidates — the demo still expands).
type demoOrchestrator struct {
	db       *sql.DB
	aud      *audiencepg.Store
	resolver identityResolver
	bus      events.EventBus
	log      *slog.Logger
}

// resetDemo makes the demo repeatable and deterministic: it wipes the demo
// account's segments/members/edges/clusters and re-seeds the four synthetic
// edges + the empty "Demo Newsletter" segment. Everything is keyed on the
// isolated demo account so it can never touch a real tenant.
func (o *demoOrchestrator) resetDemo(ctx context.Context) error {
	acct := demoAccountID()

	// Idempotent account row so the demo's segments have a valid FK target.
	if _, err := o.db.ExecContext(ctx, `
INSERT INTO accounts (id, name, email, type, status, created_at, updated_at)
VALUES ($1, $2, $3, 'advertiser', 'active', now(), now())
ON CONFLICT (id) DO NOTHING`,
		acct, demoAccountName, "demo-onboarding@synthetic.local"); err != nil {
		return fmt.Errorf("upsert demo account: %w", err)
	}

	// Wipe prior segments + their members (members cascade via segment_id).
	segID := idgen.Derive("segment", acct+"/"+demoSegmentName)
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
	ids := demoAllIDs()
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
		{UserID: demoEmailID, LinkedID: demoCookieID, Source: identity.SourceHashedEmail, LinkType: identity.LinkCrossDevice, Confidence: 1.0},
		{UserID: demoEmailID, LinkedID: demoIfaID, Source: identity.SourceDeviceID, LinkType: identity.LinkCrossDevice, Confidence: 1.0},
		{UserID: demoEmailID, LinkedID: demoHouseholdID, Source: identity.SourceHousehold, LinkType: identity.LinkHousehold, Confidence: identity.HouseholdConfidence},
	}
	if _, err := store.LinkIdentity(ctx, edges); err != nil {
		return fmt.Errorf("seed demo edges: %w", err)
	}

	// (Re)create the empty plain segment through the real audience store — no
	// members yet; the UPLOAD step adds the single email.
	if _, err := o.aud.UpsertSegment(ctx, acct, demoSegmentName, "first_party", "demo_upload", "dsp_private"); err != nil {
		return fmt.Errorf("create demo segment %q (%s): %w", demoSegmentName, segID, err)
	}
	return nil
}

// run executes the reset then the 5 steps synchronously and returns the
// assembled timeline.
func (o *demoOrchestrator) run(ctx context.Context) (demoResponse, error) {
	acct := demoAccountID()
	if err := o.resetDemo(ctx); err != nil {
		return demoResponse{}, err
	}

	steps := make([]demoStep, 0, 5)

	// --- Step 1: SETUP — the synthetic person's four identity edges. ---
	edges := []demoEdge{
		{From: demoEmailID, To: demoCookieID, Source: identity.SourceHashedEmail, LinkType: identity.LinkCrossDevice, Confidence: 1.0},
		{From: demoEmailID, To: demoIfaID, Source: identity.SourceDeviceID, LinkType: identity.LinkCrossDevice, Confidence: 1.0},
		{From: demoEmailID, To: demoHouseholdID, Source: identity.SourceHousehold, LinkType: identity.LinkHousehold, Confidence: identity.HouseholdConfidence, Household: true},
	}
	steps = append(steps, demoStep{
		N: 1, Title: "Setup — one synthetic person, four linked ids",
		Narration: "The identity graph already links this person's hashed email, browser cookie, mobile ad id (ifa) and household (hh:). The household edge groups the devices behind one IP — keep an eye on it, the clusterer treats it differently.",
		Data:      map[string]interface{}{"edges": edges},
	})

	// --- Step 2: UPLOAD — 1-row list of ONLY the hashed email → plain segment. ---
	// The real audience store path, tenant-scoped to the isolated demo account.
	upReq := audienceUploadRequest{
		AccountID: acct, Name: demoSegmentName, Type: "first_party",
		Visibility: "dsp_private", Source: "demo_upload",
	}
	upIDs := []events.ProfileSignalID{{IDType: "hashed_email", IDValue: demoEmailID}}
	upResp, err := runAudienceUpload(ctx, o.aud, nil, o.bus, o.log, upReq, upIDs)
	if err != nil {
		return demoResponse{}, fmt.Errorf("demo upload: %w", err)
	}
	steps = append(steps, demoStep{
		N: 2, Title: "Upload — a 1-row list, email only",
		Narration: "An advertiser uploads a list containing ONLY the hashed email as a plain \"Demo Newsletter\" segment. It normalises to one profile_signals-shaped row and one segment member — just the email, nothing else yet.",
		Data: map[string]interface{}{
			"segment_id":    upResp.SegmentID,
			"members_added": upResp.MembersAdded,
			"normalised_row": demoUploadRow{
				SegmentName: demoSegmentName, IDType: "hashed_email",
				IDValue: demoEmailID, Access: "first_party", Consent: true,
			},
		},
	})

	// --- Step 3: BEFORE — profile view for the email id. ---
	before, err := buildProfileView(ctx, o.db, o.resolver, demoEmailID)
	if err != nil {
		return demoResponse{}, fmt.Errorf("demo before-view: %w", err)
	}
	beforeMembers := demoSegmentMembers(before, upResp.SegmentID)
	steps = append(steps, demoStep{
		N: 3, Title: "Before — segment has one member",
		Narration: "Looking up the email in the profile store: the \"Demo Newsletter\" segment has exactly one member (the email). The identity links already show the whole household — but the segment can only reach the one device that was uploaded.",
		Data:      map[string]interface{}{"profile": before, "segment_members": beforeMembers},
	})

	// --- Step 4: EXPAND — run the REAL profile-builder (Lake:nil). ---
	// Lake:nil skips the behavioural-rule + reconcile jobs but still runs Job 1
	// (PG clustering) + Job 2b (plain-segment cluster expansion) — the demo's
	// exact path. It runs globally over the whole graph; fine on local demo data.
	res, err := profilebuilder.Run(ctx, profilebuilder.Config{DB: o.db, Lake: nil, Bus: o.bus, Log: o.log})
	if err != nil {
		return demoResponse{}, fmt.Errorf("demo profile-builder: %w", err)
	}

	// --- Step 5: AFTER — profile view again; compute the membership diff. ---
	after, err := buildProfileView(ctx, o.db, o.resolver, demoEmailID)
	if err != nil {
		return demoResponse{}, fmt.Errorf("demo after-view: %w", err)
	}
	afterMembers := demoSegmentMembers(after, upResp.SegmentID)
	added := demoDiff(beforeMembers, afterMembers)
	expand := demoExpandResult{
		Clusters: res.Clusters, ClusterMembers: res.ClusterMembers, Expanded: res.Expanded,
		MembersBefore: beforeMembers, MembersAfter: afterMembers, Added: added,
		Excluded: demoHouseholdID + " (household) — excluded from clustering by design: a household groups the people behind one IP, it does not identify one person, so it never becomes a segment member.",
	}
	// The expansion step reports the builder outcome + the diff.
	steps = append(steps, demoStep{
		N: 4, Title: "Expand — the real profile-builder runs",
		Narration: "The batch profile-builder clusters the identity graph and pre-expands plain segments: every member's cluster siblings join. The household id is left out of the cluster on purpose. This is the exact production code, run lake-free.",
		Data:      expand,
	})
	steps = append(steps, demoStep{
		N: 5, Title: "After — the segment reaches the whole cluster",
		Narration: fmt.Sprintf("The \"Demo Newsletter\" segment now contains %d ids: %v. Expansion added %v via the identity cluster. The household id (%s) stays out — it links the person but is not a segment member.", len(afterMembers), afterMembers, added, demoHouseholdID),
		Data:      map[string]interface{}{"profile": after, "segment_members": afterMembers, "added": added},
	})

	now := time.Now().UTC()
	return demoResponse{
		Ran: true, AccountID: acct, Steps: steps, RanAt: &now,
		Summary: "normalise → cluster → expand → serve: one uploaded email is normalised to a segment member, the identity graph clusters that email with its cookie and mobile ad id (but not the household), the profile-builder pre-expands the segment across that cluster, and the ad server can now reach every device — from a list of one.",
	}, nil
}

// demoSegmentMembers pulls the sorted set of demo-segment member ids out of a
// profile view (the view lists memberships across every cluster member).
func demoSegmentMembers(v profileView, segmentID string) []string {
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

// demoDiff returns the sorted ids in after that were not in before.
func demoDiff(before, after []string) []string {
	was := map[string]bool{}
	for _, b := range before {
		was[b] = true
	}
	var out []string
	for _, a := range after {
		if !was[a] {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

func demoAllIDs() []string {
	return []string{demoEmailID, demoCookieID, demoIfaID, demoHouseholdID}
}

// demoOnboardingHandler serves both endpoints:
//
//	GET  /v1/api/demo/onboarding      (support:read)   — current/last-run state
//	POST /v1/api/demo/onboarding/run  (support:update) — reset + run, 5 steps
//
// GET reflects the demo's persisted state (the seeded edges + segment member
// count) rather than caching a last-run blob: the demo is cheap and stateless
// enough that "current state" is just what's in Postgres now. A never-run demo
// returns Ran:false with a hint.
func demoOnboardingHandler(o *demoOrchestrator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		// Permission-gate BEFORE the store check so a non-staff caller always
		// gets 403 (never a 503 that would leak whether the store is wired).
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

		if o == nil || o.db == nil {
			http.Error(w, `{"error":"demo unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		switch r.Method {
		case http.MethodGet:
			resp, err := o.currentState(r.Context())
			if err != nil {
				o.log.Error("demo onboarding state failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(resp)

		case http.MethodPost:
			resp, err := o.run(r.Context())
			if err != nil {
				o.log.Error("demo onboarding run failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			o.log.Info("demo onboarding run", "account", resp.AccountID, "steps", len(resp.Steps), "actor", claims.UserID)
			_ = json.NewEncoder(w).Encode(resp)
		}
	}
}

// currentState answers the GET: does the demo segment exist yet, and how many
// members does it have? A missing segment (or zero members) means "not run".
func (o *demoOrchestrator) currentState(ctx context.Context) (demoResponse, error) {
	acct := demoAccountID()
	segID := idgen.Derive("segment", acct+"/"+demoSegmentName)
	var members int
	err := o.db.QueryRowContext(ctx,
		`SELECT count(*) FROM audience_segment_members WHERE segment_id = $1::uuid`, segID).Scan(&members)
	if err != nil && err != sql.ErrNoRows {
		return demoResponse{}, err
	}
	if members == 0 {
		return demoResponse{Ran: false, AccountID: acct,
			Summary: "The demo has not been run yet. Click \"Run demo\" to seed a synthetic person and watch a one-email upload expand into a cross-device audience."}, nil
	}
	// Already run: rebuild the profile view so the page renders on load without
	// re-running the (repeatable) mutation.
	view, err := buildProfileView(ctx, o.db, o.resolver, demoEmailID)
	if err != nil {
		return demoResponse{}, err
	}
	seg := demoSegmentMembers(view, segID)
	return demoResponse{
		Ran: true, AccountID: acct,
		Steps: []demoStep{{
			N: 5, Title: "Last run — segment reaches the cluster",
			Narration: fmt.Sprintf("The \"Demo Newsletter\" segment currently has %d members: %v. Run the demo again to replay the full 5-step story.", len(seg), seg),
			Data:      map[string]interface{}{"profile": view, "segment_members": seg},
		}},
		Summary: "normalise → cluster → expand → serve. Run the demo to replay all five steps.",
	}, nil
}

// Package privacydelete executes and verifies Level-3 (full deletion) privacy
// requests. Deleter purges every user-data row keyed to a user across the
// platform's tables and marks the opt_out_registry row completed; Verifier
// re-checks that no residual rows remain and stamps verified_at.
//
// The cmd/privacy-delete and cmd/privacy-verify binaries wire these to Postgres
// (and, for the deleter, NATS to announce completion). They run as one-shot
// jobs — load due → purge → mark → exit — with Kubernetes scheduling the cadence.
package privacydelete

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
)

// Systems purged for a Level-3 deletion. These are the durable user-data tables
// keyed by user_id; warm caches downstream refresh on the completion event.
// The lake systems are the two user-keyed Delta tables, purged via the
// pipeline (the lake's single writer); freq_cap_blocks is the ClickHouse
// analytics table keyed by user_id.
const (
	SystemIdentityGraph      = "identity_graph"
	SystemSegmentMembers     = "audience_segment_members"
	SystemFreqCapBlocks      = "freq_cap_blocks"
	SystemCHBehaviourSignals = "clickhouse:behaviour_signals"
	SystemCHProfileSignals   = "clickhouse:profile_signals"
)

// Purge is the per-user outcome: how many rows were removed from each system.
// Extra carries the counts from ExtraPurgers (lake tables, freq_cap_blocks)
// keyed by system name.
type Purge struct {
	IdentityEdges  int
	SegmentMembers int
	Extra          map[string]int
}

// Systems lists the system names touched (a system with 0 rows removed is
// still confirmed clean).
func (p Purge) Systems() []string {
	out := []string{SystemIdentityGraph, SystemSegmentMembers}
	extras := make([]string, 0, len(p.Extra))
	for k := range p.Extra {
		extras = append(extras, k)
	}
	sort.Strings(extras)
	return append(out, extras...)
}

// ExtraPurger purges + verifies user data held OUTSIDE the Postgres tables —
// the Delta lake (via the pipeline's purge endpoints) and freq_cap_blocks in
// ClickHouse. PostgresStore runs each extra after its own transaction commits;
// an extra's failure fails the whole PurgeUser so the registry row stays
// pending and the next run retries.
type ExtraPurger interface {
	// PurgeExtra removes the user's rows, returning system name → rows removed.
	PurgeExtra(ctx context.Context, userID string) (map[string]int, error)
	// ResidualExtra returns the systems still holding rows for the user.
	ResidualExtra(ctx context.Context, userID string) ([]string, error)
}

// Store is the persistence seam for the deletion pipeline.
type Store interface {
	// PendingDeletions returns user_ids with a Level-3 opt-out not yet completed.
	PendingDeletions(ctx context.Context) ([]string, error)
	// PurgeUser deletes every row keyed to userID across the user-data tables,
	// returning per-system row counts. Runs in a single transaction.
	PurgeUser(ctx context.Context, userID string) (Purge, error)
	// MarkCompleted stamps completed_at + systems_completed on the registry row.
	MarkCompleted(ctx context.Context, userID string, p Purge) error

	// CompletedUnverified returns Level-3 user_ids that are completed but not
	// yet verified.
	CompletedUnverified(ctx context.Context) ([]string, error)
	// Residual returns the systems that STILL hold rows for userID (empty = clean).
	Residual(ctx context.Context, userID string) ([]string, error)
	// MarkVerified stamps verified_at on the registry row.
	MarkVerified(ctx context.Context, userID string) error
}

// Announcer publishes the deletion-completed signal (optional; nil = skip).
// Implemented by pkg/events.EventBus.
type Announcer interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

// Deleter processes pending Level-3 deletions.
type Deleter struct {
	Store    Store
	Announce Announcer // optional
	Subject  string    // completion subject (events.SubjectPrivacyCompleted)
	Log      *slog.Logger
}

// RunPending purges every pending Level-3 user and returns how many completed.
// A per-user failure is logged and skipped (its registry row stays incomplete,
// so the next run retries it) rather than aborting the whole batch.
func (d *Deleter) RunPending(ctx context.Context) (int, error) {
	users, err := d.Store.PendingDeletions(ctx)
	if err != nil {
		return 0, fmt.Errorf("privacydelete: list pending: %w", err)
	}
	done := 0
	for _, userID := range users {
		purge, err := d.Store.PurgeUser(ctx, userID)
		if err != nil {
			d.logf().Error("purge failed, will retry next run", "user_id", userID, "error", err)
			continue
		}
		if err := d.Store.MarkCompleted(ctx, userID, purge); err != nil {
			d.logf().Error("purge done but marking completed failed, will retry", "user_id", userID, "error", err)
			continue
		}
		d.announce(ctx, userID, purge)
		d.logf().Info("user deleted",
			"user_id", userID,
			"identity_edges_removed", purge.IdentityEdges,
			"segment_members_removed", purge.SegmentMembers)
		done++
	}
	return done, nil
}

func (d *Deleter) announce(ctx context.Context, userID string, p Purge) {
	if d.Announce == nil || d.Subject == "" {
		return
	}
	// Minimal, PII-light completion signal: the user id is already the subject
	// of the deletion; systems + counts let ops confirm coverage.
	extra := 0
	for _, n := range p.Extra {
		extra += n
	}
	payload := fmt.Sprintf(`{"schema_version":1,"user_id":%q,"identity_edges_removed":%d,"segment_members_removed":%d,"extra_rows_removed":%d}`,
		userID, p.IdentityEdges, p.SegmentMembers, extra)
	if err := d.Announce.Publish(ctx, d.Subject, []byte(payload)); err != nil {
		d.logf().Warn("deletion-completed publish failed", "user_id", userID, "error", err)
	}
}

// Verifier confirms completed deletions left no residual data.
type Verifier struct {
	Store Store
	Log   *slog.Logger
}

// RunUnverified checks every completed-but-unverified user. Returns how many
// were newly verified and how many still hold residual data.
func (v *Verifier) RunUnverified(ctx context.Context) (verified, incomplete int, err error) {
	users, err := v.Store.CompletedUnverified(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("privacydelete: list unverified: %w", err)
	}
	for _, userID := range users {
		residual, err := v.Store.Residual(ctx, userID)
		if err != nil {
			v.logf().Error("residual check failed", "user_id", userID, "error", err)
			continue
		}
		if len(residual) > 0 {
			v.logf().Error("deletion incomplete — residual data found", "user_id", userID, "systems", residual)
			incomplete++
			continue
		}
		if err := v.Store.MarkVerified(ctx, userID); err != nil {
			v.logf().Error("mark verified failed", "user_id", userID, "error", err)
			continue
		}
		v.logf().Info("deletion verified", "user_id", userID)
		verified++
	}
	return verified, incomplete, nil
}

func (d *Deleter) logf() *slog.Logger  { return orNop(d.Log) }
func (v *Verifier) logf() *slog.Logger { return orNop(v.Log) }

func orNop(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.Default()
}

package profilebuilder

// querier.go — ADR 0006 phase 2. The three profile-builder reads that used to
// pull whole lake partitions into Go and aggregate in-process now run as
// server-side GROUP BY queries against ClickHouse. Only aggregated results
// cross the wire, so memory is bounded by the result set, not the table — the
// OOM-by-design is gone.
//
// BehaviourQuerier is the seam. Config.Behaviour == nil falls back to the
// original lake reads (existing callers, tests, and a safety valve keep
// working). The three methods reproduce the EXACT semantics of evaluateRule,
// personCategories/evaluateLookalike, and reconcileProfileSignals — see each
// method's doc and the pure SQL builders below (unit-tested for parity).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// QualifiedKey is one rule-qualifying user: the key plus its NEWEST matching
// signal time. The suppression filter compares Last against the purchase
// burn-list: only a signal newer than the purchase may re-enroll the user.
type QualifiedKey struct {
	Key  string
	Last time.Time
}

// KeyCategory is one (user key, category) pair — the per-person category
// signal lookalike scoring consumes. The key mirrors the serve-path
// precedence (user_id, else household_id); category is already lower-cased and
// trimmed to match personCategories.
type KeyCategory struct {
	Key      string
	Category string
}

// SegmentIDs is one segment's reconcile input: the (account, segment) it
// belongs to and every id_value ever onboarded into it (server-side deduped).
type SegmentIDs struct {
	AccountID string
	SegmentID string
	IDValues  []string
}

// BehaviourQuerier aggregates the profile store's ClickHouse tables so the
// builder never SELECTs raw rows for these paths. Implemented over ClickHouse
// (CHBehaviourQuerier); a fake in tests asserts parity with the lake path.
type BehaviourQuerier interface {
	// QualifyingUsers returns the user keys with >= rule.MinCount matching
	// behaviour_signals rows inside the rule window — the ClickHouse
	// equivalent of evaluateRule (pre cluster-expansion). rule.accountID, when
	// set (site_visit), scopes to the pixel owner's account.
	QualifyingUsers(ctx context.Context, rule Rule, now time.Time) ([]QualifiedKey, error)

	// CategorySignals returns the distinct (user key, category) pairs across
	// behaviour_signals in the last windowDays — the input evaluateLookalike
	// folds into per-person category sets. windowDays mirrors the builder's
	// widest-rule-window lake read so lookalike sees the same rows either way.
	CategorySignals(ctx context.Context, windowDays int, now time.Time) ([]KeyCategory, error)

	// SegmentMemberships returns, per (account, segment), the deduped set of
	// onboarded id_values from profile_signals — the reconcile input. Bounded
	// by the number of distinct ids, not the raw row count.
	SegmentMemberships(ctx context.Context) ([]SegmentIDs, error)
}

// --- Pure SQL builders (unit-tested against rule semantics) ---

// qualifyingUsersSQL builds the behavioural-rule aggregation. It reproduces
// evaluateRule EXACTLY:
//   - kind = rule.Event
//   - optional equality filters (channel/campaign/publisher/tag)
//   - Category: behaviour rows carry a comma-separated `categories`; the rule
//     matches when the category is present case-insensitively (hasCategory) —
//     has(arrayMap(x -> lower(trim(x)), splitByChar(',', categories)), lower(trim)).
//   - account scope when rule.accountID is set (site_visit)
//   - observed_at >= now - window_days (the >= cutoff, matching
//     evaluateRule's `at.Before(cutoff)` skip)
//   - key = user_id, else household_id (serve-path precedence), non-empty
//   - GROUP BY key HAVING count() >= min_count
//
// Every rule-supplied value is a bound '?' param — no tag/account/category can
// inject SQL. Returns the query and its ordered args.
func qualifyingUsersSQL(rule Rule, now time.Time) (string, []any) {
	var where []string
	var args []any

	where = append(where, "kind = ?")
	args = append(args, rule.Event)

	if rule.Channel != "" {
		where = append(where, "channel = ?")
		args = append(args, rule.Channel)
	}
	if rule.CampaignID != "" {
		where = append(where, "campaign_id = ?")
		args = append(args, rule.CampaignID)
	}
	if rule.PublisherID != "" {
		where = append(where, "publisher_id = ?")
		args = append(args, rule.PublisherID)
	}
	if rule.Tag != "" {
		where = append(where, "tag = ?")
		args = append(args, rule.Tag)
	}
	if rule.Category != "" {
		// hasCategory parity: split on ',', trim + case-fold each element, and
		// test membership of the (also trimmed + folded) wanted category.
		where = append(where,
			"has(arrayMap(x -> lower(trim(BOTH ' ' FROM x)), splitByChar(',', categories)), lower(trim(BOTH ' ' FROM ?)))")
		args = append(args, rule.Category)
	}
	if rule.accountID != "" {
		where = append(where, "account_id = ?")
		args = append(args, rule.accountID)
	}

	// observed_at >= cutoff. Compute the cutoff in Go from `now` so the query
	// is deterministic and matches evaluateRule's cutoff exactly (it does not
	// use ClickHouse now()).
	cutoff := now.AddDate(0, 0, -rule.WindowDays)
	where = append(where, "observed_at >= ?")
	args = append(args, cutoff)

	// key = user_id else household_id, must be non-empty.
	const keyExpr = "if(user_id != '', user_id, household_id)"
	// last = the key's newest qualifying signal. The suppression filter needs
	// it: a purchase burn-lists a user AT a moment, and only signals NEWER
	// than that moment may re-enroll them (a genuinely new abandoned cart).
	q := fmt.Sprintf(`SELECT %s AS key, max(observed_at) AS last
FROM behaviour_signals
WHERE %s
GROUP BY key
HAVING key != '' AND count() >= ?`, keyExpr, strings.Join(where, " AND "))
	args = append(args, uint64(rule.MinCount))
	return q, args
}

// categorySignalsSQL builds the lookalike category-signal read: distinct
// (key, category) pairs over the window. It mirrors personCategories — the key
// is user_id else household_id, categories are split on ',', trimmed and
// lower-cased, and empty categories dropped. windowDays matches the builder's
// widest-rule-window lake read (evaluateLookalike consumes the same rows).
func categorySignalsSQL(windowDays int, now time.Time) (string, []any) {
	cutoff := now.AddDate(0, 0, -windowDays)
	q := `SELECT DISTINCT key, category FROM (
	SELECT if(user_id != '', user_id, household_id) AS key,
	       lower(trim(BOTH ' ' FROM arrayJoin(splitByChar(',', categories)))) AS category
	FROM behaviour_signals
	WHERE observed_at >= ?
) WHERE key != '' AND category != ''`
	return q, []any{cutoff}
}

// segmentMembershipsSQL builds the reconcile read: per (account, segment) the
// deduped id_values. groupUniqArray dedupes server-side (the in-Go path
// appended every row's id_value; AddMembers dedupes downstream, so a unique
// set is equivalent and far smaller over the wire). Empty ids are filtered so
// the result matches reconcileProfileSignals' `v == ""` skip.
func segmentMembershipsSQL() string {
	return `SELECT account_id, segment_id, groupUniqArray(id_value)
FROM profile_signals
WHERE account_id != '' AND segment_id != '' AND id_value != ''
GROUP BY account_id, segment_id`
}

// --- ClickHouse implementation ---

// CHBehaviourQuerier runs the three reads against ClickHouse over the pure-Go
// native driver (database/sql handle, no CGO). It owns its connection; call
// Close on shutdown.
type CHBehaviourQuerier struct {
	db *sql.DB
}

var _ BehaviourQuerier = (*CHBehaviourQuerier)(nil)

// CHConfig configures the ClickHouse connection (mirrors the reporting
// service's ClickHouse keys — same server, same database).
type CHConfig struct {
	Addrs    []string
	Database string
	Username string
	Password string
}

// NewCHBehaviourQuerier opens and pings a ClickHouse connection.
func NewCHBehaviourQuerier(cfg CHConfig) (*CHBehaviourQuerier, error) {
	db := clickhouse.OpenDB(&clickhouse.Options{
		Addr: cfg.Addrs,
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.Username,
			Password: cfg.Password,
		},
		DialTimeout: 5 * time.Second,
	})
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping clickhouse: %w", err)
	}
	return &CHBehaviourQuerier{db: db}, nil
}

// Close releases the connection pool.
func (q *CHBehaviourQuerier) Close() error {
	if q.db == nil {
		return nil
	}
	return q.db.Close()
}

func (q *CHBehaviourQuerier) QualifyingUsers(ctx context.Context, rule Rule, now time.Time) ([]QualifiedKey, error) {
	query, args := qualifyingUsersSQL(rule, now)
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("qualifying users: %w", err)
	}
	defer rows.Close()
	var out []QualifiedKey
	for rows.Next() {
		var k QualifiedKey
		if err := rows.Scan(&k.Key, &k.Last); err != nil {
			return nil, fmt.Errorf("scan qualifying user: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (q *CHBehaviourQuerier) CategorySignals(ctx context.Context, windowDays int, now time.Time) ([]KeyCategory, error) {
	query, args := categorySignalsSQL(windowDays, now)
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("category signals: %w", err)
	}
	defer rows.Close()
	var out []KeyCategory
	for rows.Next() {
		var kc KeyCategory
		if err := rows.Scan(&kc.Key, &kc.Category); err != nil {
			return nil, fmt.Errorf("scan category signal: %w", err)
		}
		out = append(out, kc)
	}
	return out, rows.Err()
}

func (q *CHBehaviourQuerier) SegmentMemberships(ctx context.Context) ([]SegmentIDs, error) {
	rows, err := q.db.QueryContext(ctx, segmentMembershipsSQL())
	if err != nil {
		return nil, fmt.Errorf("segment memberships: %w", err)
	}
	defer rows.Close()
	var out []SegmentIDs
	for rows.Next() {
		var s SegmentIDs
		if err := rows.Scan(&s.AccountID, &s.SegmentID, &s.IDValues); err != nil {
			return nil, fmt.Errorf("scan segment membership: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

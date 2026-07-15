package reportjobs

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// TableSegmentMembers is the pseudo-table name that routes a report job to
// the segment-export path instead of the reporting query API. The job's
// filters carry segment_id; everything else (queue, artifact, status,
// download, portal UI) is the standard report-job machinery.
const TableSegmentMembers = "segment_members"

// SegmentMembersFunc resolves a segment-export job into rows.
type SegmentMembersFunc func(ctx context.Context, accountID, segmentID string) (analytics.QueryResult, error)

// PGSegmentMembers exports an account's segment memberships straight from
// Postgres. Tenant-scoped twice: the segment must belong to accountID, and
// member rows are filtered by it (RLS is the backstop).
func PGSegmentMembers(db *sql.DB) SegmentMembersFunc {
	return func(ctx context.Context, accountID, segmentID string) (analytics.QueryResult, error) {
		var res analytics.QueryResult
		if segmentID == "" {
			return res, fmt.Errorf("segment export: filters.segment_id required")
		}
		rows, err := db.QueryContext(ctx, `
SELECT m.user_id, s.name, s.visibility, m.added_at
FROM audience_segment_members m
JOIN audience_segments s ON s.id = m.segment_id
WHERE m.segment_id = $1::uuid AND m.account_id = $2::uuid AND s.account_id = $2::uuid
ORDER BY m.user_id`, segmentID, accountID)
		if err != nil {
			return res, fmt.Errorf("segment export query: %w", err)
		}
		defer rows.Close()
		res.Columns = []string{"user_id", "segment_name", "visibility", "added_at"}
		for rows.Next() {
			var userID, name, visibility string
			var addedAt sql.NullTime
			if err := rows.Scan(&userID, &name, &visibility, &addedAt); err != nil {
				return res, fmt.Errorf("segment export scan: %w", err)
			}
			added := any(nil)
			if addedAt.Valid {
				added = addedAt.Time.UTC()
			}
			res.Rows = append(res.Rows, []interface{}{userID, name, visibility, added})
		}
		return res, rows.Err()
	}
}

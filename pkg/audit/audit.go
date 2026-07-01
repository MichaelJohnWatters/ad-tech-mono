// Package audit writes immutable audit-log entries for mutating actions
// (campaign/placement CRUD, config changes). The audit_log table is
// append-only (UPDATE/DELETE revoked in migration 013); this package is the
// single write path so every mutation records who did what to which resource.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Entry is one audit record. AccountID may be empty for platform-level actions
// (written as NULL). Changes is marshalled to JSON (e.g. the patch body or a
// before/after diff); nil leaves the column at its default.
type Entry struct {
	AccountID    string
	ActorID      string // who: "apikey:<name>" or "user:<id>"
	ActorIP      string // optional
	Action       string // e.g. "campaign:update", "placement:delete"
	ResourceType string // "line_item", "placement"
	ResourceID   string
	Changes      any
	Reason       string
}

// Log appends an audit entry. Best-effort by convention: the mutation has
// already committed, so callers log-and-continue on error rather than failing
// the request. A nil db is a no-op (db-less dev / single-process tests).
func Log(ctx context.Context, db *sql.DB, e Entry) error {
	if db == nil {
		return nil
	}
	var changesArg any
	if e.Changes != nil {
		b, err := json.Marshal(e.Changes)
		if err != nil {
			return fmt.Errorf("marshal audit changes: %w", err)
		}
		changesArg = string(b)
	}
	var acct any
	if e.AccountID != "" {
		acct = e.AccountID
	}
	var ip any
	if e.ActorIP != "" {
		ip = e.ActorIP
	}
	var reason any
	if e.Reason != "" {
		reason = e.Reason
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO audit_log (account_id, actor_id, actor_ip, action, resource_type, resource_id, changes, reason, timestamp)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`,
		acct, e.ActorID, ip, e.Action, e.ResourceType, e.ResourceID, changesArg, reason)
	if err != nil {
		return fmt.Errorf("write audit_log: %w", err)
	}
	return nil
}

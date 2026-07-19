// Package notifications is the per-account in-app notification store. The
// cmd/notifications consumer translates account-scoped business events
// (budget/balance depleted, campaign state changed, report ready) into rows;
// the gateway serves the portal bell + dropdown (list / unread-count /
// mark-read) from the same store. Every method is tenant-scoped by account_id.
package notifications

import (
	"context"
	"time"
)

// Notification kinds — stable programmatic keys the UI maps to an icon/colour
// and the consumer stamps per event. Free text in the DB (no CHECK) so adding
// an event → adding a kind is a one-line change, no migration.
const (
	KindBudgetDepleted  = "budget_depleted"
	KindBalanceDepleted = "balance_depleted"
	KindCampaignState   = "campaign_state"
	KindReportReady     = "report_ready"
)

// Notification is one durable per-account alert.
type Notification struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Body      string    `json:"body,omitempty"`
	RefID     string    `json:"ref_id,omitempty"`
	Read      bool      `json:"read"`
	CreatedAt time.Time `json:"created_at"`
}

// Store is the tenant-scoped persistence the consumer and gateway share. Every
// method filters by account_id; a caller can never read or mutate another
// tenant's notifications.
type Store interface {
	// Insert records a new notification for n.AccountID.
	Insert(ctx context.Context, n Notification) error
	// ListForAccount returns the account's notifications, newest first, capped
	// at limit.
	ListForAccount(ctx context.Context, accountID string, limit int) ([]Notification, error)
	// UnreadCount is the badge number — how many unread the account has.
	UnreadCount(ctx context.Context, accountID string) (int, error)
	// MarkRead marks one notification read; scoped so a caller can only touch
	// its own rows.
	MarkRead(ctx context.Context, accountID, id string) error
	// MarkAllRead marks every unread notification for the account read.
	MarkAllRead(ctx context.Context, accountID string) error
}

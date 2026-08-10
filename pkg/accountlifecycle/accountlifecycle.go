// Package accountlifecycle is the account-closure state machine (PLAN Phase 11,
// item 105: Account Closure and Data Export). An account owner requests closure,
// which opens a 30-day grace period: the account is suspended, its live
// campaigns are paused and its placements deactivated (no new spend, no new
// auctions), but the owner can still sign in and CANCEL during the window. The
// pause/deactivate is EXACTLY reversible — the ids this closure touched are
// captured on the request row so cancel restores precisely those rows, never a
// campaign the owner had already paused themselves.
//
// Final settlement + the account-scoped data purge (after the 90-day retention
// window) are the later slices; this package owns the request/grace/cancel edge.
package accountlifecycle

import (
	"context"
	"errors"
	"time"
)

// Closure statuses.
const (
	StatusGrace     = "grace"     // grace period running; reversible
	StatusCancelled = "cancelled" // owner cancelled during grace
	StatusClosed    = "closed"    // grace elapsed, account closed out (slice 3)
)

// DefaultGraceDays is the closure grace period (PLAN: 30 days).
const DefaultGraceDays = 30

// ErrAlreadyClosing is returned when a grace closure is already open for the
// account (the partial unique index enforces one at a time).
var ErrAlreadyClosing = errors.New("account already has a closure in progress")

// ErrNotClosing is returned when cancel is called but no grace closure is open.
var ErrNotClosing = errors.New("account has no closure in progress")

// ClosureRequest is one row of the closure state machine.
type ClosureRequest struct {
	ID          string    `json:"id"`
	AccountID   string    `json:"account_id"`
	Status      string    `json:"status"`
	Reason      string    `json:"reason,omitempty"`
	RequestedBy string    `json:"requested_by,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
	GraceEndsAt time.Time `json:"grace_ends_at"`
	// PausedLineItems / DeactivatedPlacements are the ids this closure touched,
	// captured for exact reversal on cancel.
	PausedLineItems       []string   `json:"paused_line_items,omitempty"`
	DeactivatedPlacements []string   `json:"deactivated_placements,omitempty"`
	CancelledAt           *time.Time `json:"cancelled_at,omitempty"`
	ClosedAt              *time.Time `json:"closed_at,omitempty"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

// Store persists closure requests and performs the reversible suspend /
// reactivate side effects atomically.
type Store interface {
	// RequestClosure opens a grace-period closure: suspends the account, pauses
	// its live line items and deactivates its placements (capturing their ids for
	// exact reversal), and returns the new request. Returns ErrAlreadyClosing if a
	// grace closure is already open.
	RequestClosure(ctx context.Context, accountID, requestedBy, reason string, graceDays int) (ClosureRequest, error)
	// CancelClosure reverses an open grace closure: reactivates the account and
	// restores exactly the line items / placements this closure paused. Returns
	// ErrNotClosing if nothing is open.
	CancelClosure(ctx context.Context, accountID string) (ClosureRequest, error)
	// ActiveClosure returns the account's open (grace) closure, or nil if none.
	ActiveClosure(ctx context.Context, accountID string) (*ClosureRequest, error)
}

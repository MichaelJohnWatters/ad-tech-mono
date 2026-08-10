// Package support is the customer support & dispute-resolution workflow (PLAN
// Phase 11, item 108). Customers raise tickets (questions, technical issues, or
// billing disputes with a disputed amount); staff triage them in a queue, reply
// on a shared thread, and resolve — a billing dispute can resolve with a credit
// adjustment. Tickets are tenant-scoped (a customer sees only their own); staff
// read/resolve cross-tenant via the platform hatch.
package support

import (
	"context"
	"time"
)

// Ticket kinds.
const (
	KindQuestion       = "question"
	KindTechnical      = "technical"
	KindBillingDispute = "billing_dispute"
)

// Ticket statuses.
const (
	StatusOpen     = "open"     // awaiting staff
	StatusPending  = "pending"  // awaiting customer (staff replied)
	StatusResolved = "resolved" // closed with a resolution
	StatusClosed   = "closed"
)

// Author types on the message thread.
const (
	AuthorCustomer = "customer"
	AuthorStaff    = "staff"
)

// IsValidKind reports whether k is a ticket kind.
func IsValidKind(k string) bool {
	return k == KindQuestion || k == KindTechnical || k == KindBillingDispute
}

// Ticket is one support/dispute case.
type Ticket struct {
	ID                   string     `json:"id"`
	AccountID            string     `json:"account_id"`
	Kind                 string     `json:"kind"`
	Subject              string     `json:"subject"`
	Status               string     `json:"status"`
	AmountDisputedMicros *int64     `json:"amount_disputed_micros,omitempty"`
	Currency             string     `json:"currency"`
	Resolution           string     `json:"resolution,omitempty"`
	AssignedTo           string     `json:"assigned_to,omitempty"`
	CreatedBy            string     `json:"created_by,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	ResolvedAt           *time.Time `json:"resolved_at,omitempty"`
	// AccountName is joined on the staff queue view (never on the customer's own).
	AccountName string `json:"account_name,omitempty"`
	// Messages is populated on the detail read.
	Messages []Message `json:"messages,omitempty"`
}

// Message is one turn on a ticket's thread.
type Message struct {
	ID         string    `json:"id"`
	TicketID   string    `json:"ticket_id"`
	AuthorType string    `json:"author_type"`
	AuthorID   string    `json:"author_id,omitempty"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
}

// RedactForCustomer strips internal staff identity from a ticket before it is
// returned to a customer: the assignee and every STAFF message's author id (the
// customer sees "Support" via author_type, never the staff member's JWT
// subject). Migration 094's guarantee. Staff reads (GetAny/ListAll) keep them.
func RedactForCustomer(t *Ticket) {
	if t == nil {
		return
	}
	t.AssignedTo = ""
	for i := range t.Messages {
		if t.Messages[i].AuthorType == AuthorStaff {
			t.Messages[i].AuthorID = ""
		}
	}
}

// RedactListForCustomer redacts the staff-identity fields on each ticket in a
// customer's list view.
func RedactListForCustomer(tickets []Ticket) {
	for i := range tickets {
		RedactForCustomer(&tickets[i])
	}
}

// Store persists tickets + their message threads. Customer-facing methods are
// tenant-scoped (RLS GUC); the staff queue + resolve run under the platform hatch.
type Store interface {
	// Create opens a ticket for the account (tenant), with an optional opening
	// message. Returns the ticket id.
	Create(ctx context.Context, t Ticket, openingMessage string) (string, error)
	// ListByAccount returns the account's own tickets, newest first (tenant).
	ListByAccount(ctx context.Context, accountID string, limit int) ([]Ticket, error)
	// ListAll returns every tenant's tickets for the staff queue, newest first,
	// joined to the account name (platform hatch).
	ListAll(ctx context.Context, limit int) ([]Ticket, error)
	// GetForAccount returns one of the account's tickets with its thread (tenant);
	// nil when absent or not the account's.
	GetForAccount(ctx context.Context, accountID, id string) (*Ticket, error)
	// GetAny returns any ticket with its thread for staff (platform hatch).
	GetAny(ctx context.Context, id string) (*Ticket, error)
	// AddCustomerMessage appends a customer reply (tenant) and reopens the ticket.
	AddCustomerMessage(ctx context.Context, accountID, ticketID, authorID, body string) error
	// AddStaffMessage appends a staff reply (platform hatch) and marks pending.
	AddStaffMessage(ctx context.Context, ticketID, authorID, body string) error
	// Resolve sets a ticket's status/resolution/assignee (platform hatch, staff).
	// Returns the fresh ticket, or nil if the id is unknown.
	Resolve(ctx context.Context, ticketID, status, resolution, assignedTo string) (*Ticket, error)
}

// Package partner is the external-partner onboarding registry (PLAN Phase 11,
// item 112). An external integration partner — a demand DSP that bids into our
// exchange, or a supply SSP that sends us bid requests — is a first-class record
// with an onboarding lifecycle (pending -> sandbox -> certified -> active, with
// paused/terminated off-ramps) instead of a naked exchange.dsp_endpoints config
// string. This slice is the staff-managed registry; the partner-facing self-serve
// portal, sandbox keys, test-bid, and certification suite build on it.
package partner

import (
	"context"
	"time"
)

// Kinds of integration partner.
const (
	KindDSP = "dsp" // demand: bids into our exchange
	KindSSP = "ssp" // supply: sends us bid requests
)

// Onboarding lifecycle statuses.
const (
	StatusPending    = "pending"    // registered, not yet testing
	StatusSandbox    = "sandbox"    // test traffic, fake money
	StatusCertified  = "certified"  // passed the acceptance suite
	StatusActive     = "active"     // live in production
	StatusPaused     = "paused"     // temporarily off
	StatusTerminated = "terminated" // permanently off
)

// IsValidKind reports whether k is a partner kind.
func IsValidKind(k string) bool { return k == KindDSP || k == KindSSP }

// IsValidAuthMethod reports whether m is a supported partner auth method (mirrors
// the migration's CHECK, so the handler can reject a bad value with a 400 rather
// than letting it 500 on the constraint).
func IsValidAuthMethod(m string) bool {
	return m == "api_key" || m == "mtls" || m == "none"
}

// IsValidStatus reports whether s is a lifecycle status.
func IsValidStatus(s string) bool {
	switch s {
	case StatusPending, StatusSandbox, StatusCertified, StatusActive, StatusPaused, StatusTerminated:
		return true
	}
	return false
}

// allowedTransitions is the onboarding state machine. A partner advances
// register -> sandbox -> certified -> active; can be paused from sandbox/certified/
// active and resumed; and can be terminated from anywhere. Enforcing this keeps a
// partner from being flipped straight to active without passing certification.
var allowedTransitions = map[string][]string{
	StatusPending:    {StatusSandbox, StatusTerminated},
	StatusSandbox:    {StatusCertified, StatusPaused, StatusTerminated},
	StatusCertified:  {StatusActive, StatusSandbox, StatusPaused, StatusTerminated},
	StatusActive:     {StatusPaused, StatusTerminated},
	StatusPaused:     {StatusSandbox, StatusCertified, StatusActive, StatusTerminated},
	StatusTerminated: {}, // terminal
}

// CanTransition reports whether a partner may move from -> to.
func CanTransition(from, to string) bool {
	if !IsValidStatus(from) || !IsValidStatus(to) {
		return false
	}
	for _, s := range allowedTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Partner is a registered external integration partner.
type Partner struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Kind           string     `json:"kind"`
	Status         string     `json:"status"`
	EndpointBid    string     `json:"endpoint_bid"`
	EndpointNURL   string     `json:"endpoint_nurl,omitempty"`
	Seat           string     `json:"seat,omitempty"`
	AuthMethod     string     `json:"auth_method"`
	AuthSecretRef  string     `json:"auth_secret_ref,omitempty"`
	Channels       []string   `json:"channels"`
	Formats        []string   `json:"formats"`
	TimeoutMs      int        `json:"timeout_ms"`
	MaxQPS         *int       `json:"max_qps,omitempty"`
	OpenRTBVersion string     `json:"openrtb_version"`
	ContactTech    string     `json:"contact_tech,omitempty"`
	ContactBilling string     `json:"contact_billing,omitempty"`
	Notes          string     `json:"notes,omitempty"`
	CreatedBy      string     `json:"created_by,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	OnboardedAt    *time.Time `json:"onboarded_at,omitempty"`
}

// Input is a create/register request. Status is not client-set (always starts
// pending); id/timestamps are server-owned.
type Input struct {
	Name           string   `json:"name"`
	Kind           string   `json:"kind"`
	EndpointBid    string   `json:"endpoint_bid"`
	EndpointNURL   string   `json:"endpoint_nurl"`
	Seat           string   `json:"seat"`
	AuthMethod     string   `json:"auth_method"`
	Channels       []string `json:"channels"`
	Formats        []string `json:"formats"`
	TimeoutMs      int      `json:"timeout_ms"`
	MaxQPS         *int     `json:"max_qps"`
	OpenRTBVersion string   `json:"openrtb_version"`
	ContactTech    string   `json:"contact_tech"`
	ContactBilling string   `json:"contact_billing"`
	Notes          string   `json:"notes"`
}

// Store is the partner registry persistence. It is platform-global (staff-managed
// in this slice); every method is called behind the gateway's staff RBAC gate.
type Store interface {
	// Create registers a new partner (status pending). createdBy is the staff
	// JWT subject.
	Create(ctx context.Context, in Input, createdBy string) (Partner, error)
	// List returns all partners, newest first. If status != "" it filters by it.
	List(ctx context.Context, status string) ([]Partner, error)
	// Get returns one partner by id.
	Get(ctx context.Context, id string) (Partner, error)
	// SetStatus transitions a partner's lifecycle status (validated by the caller
	// against CanTransition). Sets onboarded_at the first time it goes active.
	SetStatus(ctx context.Context, id, status string) (Partner, error)
	// Update edits a partner's mutable metadata (endpoints/contacts/limits).
	Update(ctx context.Context, id string, in Input) (Partner, error)
}

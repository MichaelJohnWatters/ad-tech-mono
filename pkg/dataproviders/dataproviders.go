// Package dataproviders is the tenant-scoped data-provider registry (ADR 0009) —
// the first-class DMP entity that promotes the free-text audience_ingest_jobs
// `provider` string into a real record. A provider carries its data-party
// classification, licence, default id_type, encryption contract, and completion-
// email defaults; audience ingestion resolves those from the provider (when a
// job references one) instead of re-deriving them ad hoc.
//
// Every read/write is tenant-scoped (explicit account_id filter) and runs under
// the RLS context (app.current_account_id), mirroring pkg/audiencemappings and
// pkg/reportjobs. Providers are per-account; the Scope field is the seam for a
// future platform-global catalogue (the deferred DMP-monetization layer).
package dataproviders

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Party is the data-party classification: first / second / third party. Set once
// per provider (default_party) and stamped onto ingested segments/signals.
const (
	PartyFirst  = "first"
	PartySecond = "second"
	PartyThird  = "third"
)

// Licence is the permitted-use provenance recorded on signals.
const (
	LicenceFirstParty = "first_party"
	LicencePurchased  = "purchased"
	LicenceBarter     = "barter"
)

// Provider kinds. The kind drives the default party when one isn't set explicitly
// (see DefaultPartyForKind).
const (
	KindCRM    = "crm"
	KindDMP    = "dmp"
	KindCDP    = "cdp"
	KindAgency = "agency"
	KindOther  = "other"
)

var (
	validKinds    = map[string]bool{KindCRM: true, KindDMP: true, KindCDP: true, KindAgency: true, KindOther: true}
	validParties  = map[string]bool{PartyFirst: true, PartySecond: true, PartyThird: true}
	validLicences = map[string]bool{LicenceFirstParty: true, LicencePurchased: true, LicenceBarter: true}
	validStatuses = map[string]bool{"active": true, "paused": true}
)

// Provider is one tenant-owned data source.
type Provider struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	// DefaultParty is stamped onto segments/signals ingested from this provider
	// unless the upload overrides it.
	DefaultParty string `json:"default_party"`
	// DefaultLicence is the permitted-use provenance recorded on signals.
	DefaultLicence string `json:"default_licence"`
	// DefaultIDType is stamped on rows without an explicit id_type column.
	DefaultIDType string `json:"default_id_type"`
	// EncryptionExpected rejects a cleartext (non-PGP) file from this provider on
	// ingest (Phase 4). The platform PGP decrypt key is unaffected.
	EncryptionExpected bool `json:"encryption_expected"`
	// NotifyEmails are default completion-email recipients for this provider's
	// ingests, merged with the uploader + per-upload additional_emails.
	NotifyEmails []string  `json:"notify_emails"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Store is the tenant-scoped provider registry. Every method binds to an account.
type Store interface {
	// Create inserts or (on name conflict) updates the account's provider and
	// returns its id. Bind p.AccountID to the authenticated caller before calling.
	Create(ctx context.Context, p Provider) (string, error)
	// ListByAccount returns the account's providers, newest first.
	ListByAccount(ctx context.Context, accountID string) ([]Provider, error)
	// GetByAccount returns one provider only if it belongs to the account
	// (nil, nil when absent — callers 404).
	GetByAccount(ctx context.Context, accountID, id string) (*Provider, error)
	// DeleteByAccount removes one provider scoped to the account.
	DeleteByAccount(ctx context.Context, accountID, id string) error
}

// ErrNotFound is returned (via the nil, nil convention) when a provider is absent.
var ErrNotFound = errors.New("data provider not found")

// DefaultPartyForKind maps a provider kind to the party it implies when
// default_party isn't set explicitly: a CRM is your own 1st-party data, an
// agency shares a partner's 1st-party (2nd), a DMP/CDP is purchased/aggregated
// (3rd). "other" defaults to first (the conservative, no-syndication choice).
func DefaultPartyForKind(kind string) string {
	switch kind {
	case KindDMP, KindCDP:
		return PartyThird
	case KindAgency:
		return PartySecond
	default:
		return PartyFirst
	}
}

// DefaultLicenceForParty maps a party to the licence it implies: 1st party is
// first_party, everything sourced externally is purchased by default (barter is
// an explicit opt-in).
func DefaultLicenceForParty(party string) string {
	if party == PartyFirst {
		return LicenceFirstParty
	}
	return LicencePurchased
}

// Normalize fills defaults and lowercases the enum fields, so callers can pass a
// partial Provider (name + kind) and get a coherent record. It does NOT validate
// — call Validate for that.
func Normalize(p *Provider) {
	p.Name = strings.TrimSpace(p.Name)
	p.Kind = strings.ToLower(strings.TrimSpace(p.Kind))
	if p.Kind == "" {
		p.Kind = KindOther
	}
	p.DefaultParty = strings.ToLower(strings.TrimSpace(p.DefaultParty))
	if p.DefaultParty == "" {
		p.DefaultParty = DefaultPartyForKind(p.Kind)
	}
	p.DefaultLicence = strings.ToLower(strings.TrimSpace(p.DefaultLicence))
	if p.DefaultLicence == "" {
		p.DefaultLicence = DefaultLicenceForParty(p.DefaultParty)
	}
	p.DefaultIDType = strings.TrimSpace(p.DefaultIDType)
	if p.DefaultIDType == "" {
		p.DefaultIDType = "user_id"
	}
	p.Status = strings.ToLower(strings.TrimSpace(p.Status))
	if p.Status == "" {
		p.Status = "active"
	}
	if p.NotifyEmails == nil {
		p.NotifyEmails = []string{}
	}
}

// Validate enforces the enum constraints (matching the migration CHECKs) and
// requires a name. Returns a human-readable reason on failure.
func Validate(p Provider) error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("provider name is required")
	}
	if !validKinds[p.Kind] {
		return fmt.Errorf("invalid kind %q (allowed: crm, dmp, cdp, agency, other)", p.Kind)
	}
	if !validParties[p.DefaultParty] {
		return fmt.Errorf("invalid default_party %q (allowed: first, second, third)", p.DefaultParty)
	}
	if !validLicences[p.DefaultLicence] {
		return fmt.Errorf("invalid default_licence %q (allowed: first_party, purchased, barter)", p.DefaultLicence)
	}
	if !validStatuses[p.Status] {
		return fmt.Errorf("invalid status %q (allowed: active, paused)", p.Status)
	}
	return nil
}

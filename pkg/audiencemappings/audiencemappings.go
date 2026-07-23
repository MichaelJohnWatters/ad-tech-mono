// Package audiencemappings is the tenant-scoped custom field-mapping store
// ("connectors", ADR 0008 Feature 3). A provider ships audience files in an
// arbitrary column layout; a tenant BUILDS a named, reusable mapping (their
// lowercased source column → our canonical field) from a small sample upload,
// then APPLIES it on a real upload — the gateway loads the mapping and sets
// SegmentSpec.FieldMappings from it before the shared pkg/ingest processor runs.
//
// The mapping TARGETS are intentionally narrow: only the fields we actually
// consume (id_value required, id_type). A provider can't map — and we never
// store — columns we don't use. This is the "prevent needless data" guarantee
// (the processor already projects each row to id_value/id_type; unmapped source
// columns are dropped and never persisted).
//
// Every read/write is tenant-scoped (explicit account_id filter) and runs under
// the RLS context (app.current_account_id), mirroring pkg/reportjobs.
package audiencemappings

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ConsumedFields is the ONLY set of canonical targets a mapping VALUE may point
// at — the fields the ingest processor actually consumes. Widening this is a
// deliberate future change (ADR 0008), never provider-configurable.
var ConsumedFields = []string{"id_value", "id_type"}

// FieldIDValue is the required target: at least one source column must map to it,
// else the file has no usable id and the mapping is rejected.
const FieldIDValue = "id_value"

// ErrNotFound is returned (via nil, nil convention) when a mapping is absent for
// the account; callers 404. Retained for symmetry with other stores.
var ErrNotFound = errors.New("mapping not found")

// Mapping is one saved connector: a tenant's named source→canonical column map.
type Mapping struct {
	ID        string            `json:"id"`
	AccountID string            `json:"account_id"`
	Name      string            `json:"name"`
	// Mappings is their lowercased source column → canonical field (a value in
	// ConsumedFields). At least one value must be id_value.
	Mappings  map[string]string `json:"mappings"`
	IDType    string            `json:"id_type"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// Store is the tenant-scoped mapping registry. Every method binds to an account.
type Store interface {
	// Create inserts or (on name conflict) updates the account's mapping and
	// returns its id. Bind m.AccountID to the authenticated caller before calling.
	Create(ctx context.Context, m Mapping) (string, error)
	// ListByAccount returns the account's mappings, newest first.
	ListByAccount(ctx context.Context, accountID string) ([]Mapping, error)
	// GetByAccount returns one mapping only if it belongs to the account
	// (nil, nil when absent — callers 404).
	GetByAccount(ctx context.Context, accountID, id string) (*Mapping, error)
	// DeleteByAccount removes one mapping scoped to the account.
	DeleteByAccount(ctx context.Context, accountID, id string) error
}

// consumed reports whether target is a canonical field we consume.
func consumed(target string) bool {
	for _, f := range ConsumedFields {
		if f == target {
			return true
		}
	}
	return false
}

// ValidateMapping enforces the "prevent needless data" guarantee: every VALUE
// (canonical target) must be in ConsumedFields, and at least one source column
// must map to id_value (else no usable id). An empty/whitespace source column or
// target is rejected. Returns a human-readable reason on failure.
func ValidateMapping(m Mapping) error {
	if len(m.Mappings) == 0 {
		return errors.New("mapping is empty — map at least one column to id_value")
	}
	hasIDValue := false
	for src, target := range m.Mappings {
		if src == "" {
			return errors.New("mapping has an empty source column")
		}
		if !consumed(target) {
			return fmt.Errorf("column %q maps to %q, which is not a consumed field (allowed: %v)", src, target, ConsumedFields)
		}
		if target == FieldIDValue {
			hasIDValue = true
		}
	}
	if !hasIDValue {
		return errors.New("no column maps to id_value — a mapping must identify the id column")
	}
	return nil
}

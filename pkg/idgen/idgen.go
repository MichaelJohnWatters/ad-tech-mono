// Package idgen derives deterministic UUIDs from external string keys.
//
// Used wherever code or YAML refers to entities by friendly names (li-001,
// adv-acme, …): the seed binary writes Derive("line_item", "li-001") into
// Postgres, and any service that needs to look up the same row regenerates
// the same UUID at runtime. Single function, single namespace — change either
// and every derived ID changes (intentional kill-switch).
package idgen

import "github.com/google/uuid"

// namespace anchors every derivation. Itself a UUIDv5 of the platform domain
// so the value is deterministic without a hardcoded UUID literal.
var namespace = uuid.NewSHA1(uuid.NameSpaceDNS, []byte("adtech-mono.platform"))

// Derive returns a UUIDv5 text representation for kind:externalKey.
//
//	idgen.Derive("line_item", "li-001")   → "f3e5...-..." (always the same)
//	idgen.Derive("account",   "adv-acme") → "..."
func Derive(kind, externalKey string) string {
	return uuid.NewSHA1(namespace, []byte(kind+":"+externalKey)).String()
}

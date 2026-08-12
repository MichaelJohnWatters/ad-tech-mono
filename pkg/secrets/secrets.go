// Package secrets is the platform-managed credential store. Holds opaque
// auth material (JWT signing keys, HMAC secrets, partner shared secrets,
// API keys, service-to-service shared secrets) loaded into a warm cache
// per service. Rotations propagate via NATS invalidate within milliseconds.
//
// Read path: services use pkg/cache/warm.Cache[secrets.Secret] with this
// package's models + lookup helpers. The pkg/store/postgres.SecretLoader
// supplies the rows. See docs/PLAN.md → "Auth Infrastructure".
//
// Schema mirrors migrations/027_secrets.sql. Status semantics:
//
//	active   — currently the canonical value; validators MUST accept
//	rotating — old value during a grace window after rotation; validators
//	           MUST accept these too so partners + in-flight requests
//	           don't break instantly when an operator rotates
//	revoked  — no longer accepted by anyone; kept in the table for audit
//	           but filtered out of warm-cache loads
package secrets

import "time"

// Purpose taxonomy. New purposes need a migration to extend the CHECK
// constraint AND the per-service filter helpers below.
const (
	PurposeJWTSigning     = "jwt_signing"     // JWT HS256/RS256 keys
	PurposeHMACTracker    = "hmac_tracker"    // browser-pixel HMAC secrets (platform key)
	PurposeHMACConversion = "hmac_conversion" // per-advertiser S2S conversion-postback HMAC key (G7)
	PurposePartnerShared  = "partner_shared"  // external partners (Prebid, S2S)
	PurposePartnerSandbox = "partner_sandbox" // per-partner self-serve sandbox API key (#112)
	PurposeServiceS2S     = "service_s2s"     // internal service-to-service
	PurposeAPIKey         = "api_key"         // operator-managed CRUD API keys
	PurposePGPPrivate     = "pgp_private"     // armored OpenPGP private key: audience-file decrypt-on-ingest (ADR 0008)
	PurposeAdCertEd25519  = "adcert_ed25519"  // Ed25519 private key: signing outbound OpenRTB bid requests (Phase I)
)

// Status values.
const (
	StatusActive   = "active"
	StatusRotating = "rotating"
	StatusRevoked  = "revoked"
)

// Owner sentinel for cross-service secrets (e.g. JWT signing key the
// gateway issues and every service validates against).
const OwnerPlatform = "platform"

// Secret is the warm-cache row. Mirrors the secrets table.
type Secret struct {
	ID        string
	Name      string
	Value     string
	Purpose   string
	Owner     string
	AccountID string // "" = platform-wide; non-empty = scoped to one advertiser (G7 hmac_conversion)
	Status    string
	RotatedAt *time.Time
	RevokesAt *time.Time
	ExpiresAt *time.Time
}

// IsAcceptable reports whether a validator should accept this secret.
// Active + rotating both validate; revoked does not. Natural expiry
// (expires_at) also fails the check independent of status — operators
// may set an expiry on a partner key without going through the rotation
// flow.
func (s Secret) IsAcceptable(now time.Time) bool {
	if s.Status == StatusRevoked {
		return false
	}
	if s.ExpiresAt != nil && now.After(*s.ExpiresAt) {
		return false
	}
	return true
}

// FilterFor returns the (purpose, owner) tuples a given service needs
// to load into its warm cache. Filter narrowly: gateway doesn't need
// tracker HMAC keys; exchange doesn't need SSP's JWT signing key.
//
// Returned in a shape SecretLoader can use directly in a SQL IN clause.
//
// To add a new service to the filter map: append the purposes that
// service's middleware reads. Each service should only see what it
// validates — smaller cache, smaller blast radius if any one row leaks.
type Filter struct {
	Purposes []string
	Owners   []string // empty = match platform + this service only
}

// FilterFor returns the load filter for a service. Empty filter (no purpose
// in the map) means "this service doesn't need any secrets" — loader
// returns an empty result, warm cache stays empty, /readyz still passes
// because the empty load was successful.
func FilterFor(serviceName string) Filter {
	switch serviceName {
	case "gateway":
		// Gateway issues JWTs, validates incoming API keys for the dashboard /
		// config UI, and serves the platform PGP public key (ADR 0008) + decrypts
		// small inline audience uploads.
		return Filter{
			Purposes: []string{PurposeJWTSigning, PurposeAPIKey, PurposePGPPrivate},
		}
	case "dsp", "ssp":
		// CRUD endpoints validate operator API keys + JWTs issued by gateway.
		return Filter{
			Purposes: []string{PurposeJWTSigning, PurposeAPIKey},
		}
	case "exchange":
		// External Prebid partners present a shared secret; internal services may
		// eventually use service_s2s; the exchange also SIGNS outbound bid
		// requests with the active ads.cert Ed25519 key (Phase I).
		return Filter{
			Purposes: []string{PurposePartnerShared, PurposeServiceS2S, PurposeAdCertEd25519},
		}
	case "tracker":
		// HMAC sigs on every pixel URL: the platform hmac_tracker key(s) for
		// impression/click/view, PLUS every advertiser's per-account
		// hmac_conversion key so /v1/t/conv can be validated by advid (G7). The
		// conversion keys are owner='platform' rows scoped by account_id — the
		// tracker needs the whole set to validate any incoming conversion.
		return Filter{
			Purposes: []string{PurposeHMACTracker, PurposeHMACConversion},
		}
	case "adserver", "publisher-adserver":
		// SIGN pixel/click URLs with the active hmac_tracker key (the tracker
		// validates the overlap set). publisher-adserver also calls SSP/adserver
		// internally; may eventually carry an S2S header.
		return Filter{
			Purposes: []string{PurposeHMACTracker},
		}
	default:
		return Filter{}
	}
}

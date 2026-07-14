package constants

// ============================================================
// HTTP Content Types
// ============================================================

const (
	ContentTypeJSON  = "application/json"
	ContentTypeHTML  = "text/html"
	ContentTypePlain = "text/plain; charset=utf-8"
	ContentTypeYAML  = "application/yaml"
	ContentTypeGIF   = "image/gif"
)

// ============================================================
// HTTP Headers
// ============================================================

const (
	HeaderContentType   = "Content-Type"
	HeaderCacheControl  = "Cache-Control"
	HeaderAuthorization = "Authorization"
	HeaderAccountID     = "X-Account-ID"
	// HeaderAccountType carries the session's account type (advertiser,
	// publisher, agency, staff, admin) on gateway→internal proxied calls, so
	// internal services can tell an end-customer identity (scope to their
	// account) from a platform operator (unscoped). See middleware.CallerScope.
	HeaderAccountType = "X-Account-Type"
	HeaderUserID      = "X-User-ID"
	// HeaderPublisherID carries a publisher_id the gateway has VALIDATED belongs
	// to the session, for endpoints that scope by publisher (e.g. the trace
	// inspector). Internal services trust only this header for publisher scope —
	// never a client-supplied query param — so a publisher can't read another
	// publisher's data. Absent = publisher scope not resolved (deny).
	HeaderPublisherID = "X-Publisher-ID"
	// HeaderActAs names a managed account an agency session wants to act on
	// behalf of. The gateway validates it against the agency's managed
	// accounts and, if allowed, forwards that account as the effective
	// X-Account-ID (type advertiser) so downstream services scope to it.
	HeaderActAs        = "X-Act-As-Account"
	HeaderTraceID      = "X-Trace-ID"
	HeaderWebhookEvent = "X-Webhook-Event"
	HeaderWebhookSig   = "X-Webhook-Signature"

	// CORS
	HeaderCORSOrigin  = "Access-Control-Allow-Origin"
	HeaderCORSMethods = "Access-Control-Allow-Methods"
	HeaderCORSHeaders = "Access-Control-Allow-Headers"
)

// ============================================================
// Cache Control Values
// ============================================================

const (
	CacheNoStore = "no-store, no-cache"
)

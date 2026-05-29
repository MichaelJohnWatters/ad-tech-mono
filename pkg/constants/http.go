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
	HeaderUserID        = "X-User-ID"
	HeaderTraceID       = "X-Trace-ID"
	HeaderWebhookEvent  = "X-Webhook-Event"
	HeaderWebhookSig    = "X-Webhook-Signature"

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

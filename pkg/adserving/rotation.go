package adserving

import "sync/atomic"

// activeKey is the key that SignURL-based signers (the macros below,
// publisher-adserver) use to SIGN pixel/click URLs. It defaults to
// DefaultSigningKey; the signer services override it from the secrets store's
// ACTIVE hmac_tracker secret at boot and on refresh, so newly-signed pixels
// follow key rotation. The tracker, meanwhile, VALIDATES against the whole
// overlap set (active + rotating), so a pixel signed just before the switch
// still verifies until the old key is revoked. Read on the hot serve path;
// atomic so the refresh goroutine can swap it without a lock.
var activeKey atomic.Pointer[string]

// SetActiveSigningKey sets the key the signers use. An empty key is IGNORED so a
// transient empty/missing secret can never silently break signing (the current
// key — or DefaultSigningKey — stays in force).
func SetActiveSigningKey(k string) {
	if k == "" {
		return
	}
	activeKey.Store(&k)
}

// ActiveSigningKey returns the current signing key — DefaultSigningKey until a
// service overrides it via SetActiveSigningKey.
func ActiveSigningKey() string {
	if p := activeKey.Load(); p != nil {
		return *p
	}
	return DefaultSigningKey
}

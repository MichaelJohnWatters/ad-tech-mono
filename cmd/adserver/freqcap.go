package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/models"
)

// FreqCap is the Redis-backed per-user-per-campaign impression counter.
//
// AllowAndRecord atomically INCRs the counter and reports whether the
// next impression is allowed. The first INCR also sets the TTL so each
// counter expires after the cap window.
//
// The limit + window are passed per call so the serve handler can supply the
// campaign's advertiser-configured cap (from the freq-cap warm cache) and fall
// back to the platform-default live-config values otherwise.
type FreqCap struct {
	l2  cache.L2Cache
	log *slog.Logger
}

func NewFreqCap(l2 cache.L2Cache, log *slog.Logger) *FreqCap {
	return &FreqCap{l2: l2, log: log}
}

func freqCapKey(userID, campaignID string) string {
	return "adserver:freqcap:" + userID + ":" + campaignID
}

// AllowAndRecord returns true if the impression is under the cap. Increments the
// counter and sets the window TTL on the first increment of a window. Empty
// userID (no consent) or a non-positive limit bypasses the cap entirely.
//
// This is the DISPLAY path: the ad server renders the ad here, so the serve
// decision is ~the impression and check-and-increment is correct. Video/audio
// are decoupled (async conditioning + server-side stitching in SSAI, player
// re-requests), so they PEEK here (Allow) and RECORD only when the ad is
// actually stitched — see the CapMode split in the serve handler.
func (f *FreqCap) AllowAndRecord(ctx context.Context, userID, campaignID string, limit int, window time.Duration) bool {
	if userID == "" || limit <= 0 {
		return true
	}
	key := freqCapKey(userID, campaignID)
	count, err := f.l2.Incr(ctx, key)
	if err != nil {
		f.log.Warn("freqcap incr failed", "key", key, "error", err)
		return true // fail-open: never block ads on cache failure
	}
	if count == 1 {
		if err := f.l2.Expire(ctx, key, window); err != nil {
			f.log.Warn("freqcap expire failed", "key", key, "error", err)
		}
	}
	return count <= int64(limit)
}

// Allow PEEKS the counter — reports whether the NEXT impression would be under
// the cap WITHOUT incrementing. Used by the serve decision for formats whose
// impression is confirmed later (video/audio: SSAI stitches + Records on fill),
// so a nobid or a cold conditioning-miss never burns a slot. The next
// impression is allowed when the current count is strictly below the limit
// (after a Record it becomes count+1 ≤ limit). Fail-open on cache error and on
// empty userID / non-positive limit (cap bypassed), matching AllowAndRecord.
func (f *FreqCap) Allow(ctx context.Context, userID, campaignID string, limit int) bool {
	if userID == "" || limit <= 0 {
		return true
	}
	key := freqCapKey(userID, campaignID)
	v, ok, err := f.l2.Get(ctx, key)
	if err != nil {
		f.log.Warn("freqcap peek failed", "key", key, "error", err)
		return true // fail-open
	}
	if !ok || v == "" {
		return true // no impressions yet this window
	}
	count, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		f.log.Warn("freqcap peek parse failed", "key", key, "value", v, "error", perr)
		return true // fail-open on a malformed counter
	}
	return count < int64(limit)
}

// Record INCREMENTs the counter (setting the window TTL on the first increment),
// with no allow/block decision — the caller already decided via Allow. Used to
// count a video/audio impression at stitch time, so the count reflects ads
// actually served, not serve decisions. No-op on empty userID / non-positive
// limit.
func (f *FreqCap) Record(ctx context.Context, userID, campaignID string, limit int, window time.Duration) {
	if userID == "" || limit <= 0 {
		return
	}
	key := freqCapKey(userID, campaignID)
	count, err := f.l2.Incr(ctx, key)
	if err != nil {
		f.log.Warn("freqcap record incr failed", "key", key, "error", err)
		return
	}
	if count == 1 {
		if err := f.l2.Expire(ctx, key, window); err != nil {
			f.log.Warn("freqcap record expire failed", "key", key, "error", err)
		}
	}
}

// ── Combined user+household ops: ONE Redis round trip via Lua ─────────────
//
// The serve handler runs the cap for BOTH scopes on every request; as four
// serial ops (2× INCR + 2× EXPIRE) that was the whole render leg's cost
// under load (adserver freqcap phase p95 80ms vs 1-2ms for everything else,
// 2026-08-05). Each script preserves the serial path's exact semantics —
// notably check-and-record only touches the household counter when the user
// scope ALLOWED (the pre-existing asymmetry). When the backend can't script
// (in-memory fallback era, tests, Redis blip) every method degrades to the
// original per-scope calls.

// checkScript: INCR user (PEXPIRE on first); if over limit stop (household
// untouched). Else INCR household when present. Returns {userCount, hhCount};
// hhCount -1 = not evaluated (blocked at user or no household key).
const checkScript = `
local limit = tonumber(ARGV[1])
local win = tonumber(ARGV[2])
local c1 = redis.call('INCR', KEYS[1])
if c1 == 1 then redis.call('PEXPIRE', KEYS[1], win) end
if c1 > limit or #KEYS < 2 then return {c1, -1} end
local c2 = redis.call('INCR', KEYS[2])
if c2 == 1 then redis.call('PEXPIRE', KEYS[2], win) end
return {c1, c2}`

// peekScript: read every counter without touching it.
const peekScript = `
local out = {}
for i, k in ipairs(KEYS) do out[i] = tonumber(redis.call('GET', k) or '0') end
return out`

// recordScript: INCR every counter (PEXPIRE on first) — stitch-time counting.
const recordScript = `
local win = tonumber(ARGV[1])
for i, k in ipairs(KEYS) do
  local c = redis.call('INCR', k)
  if c == 1 then redis.call('PEXPIRE', k, win) end
end
return 1`

// capKeys returns the 1-2 keys for the scopes present (user first).
func capKeys(userID, householdID, campaignID string) []string {
	keys := make([]string, 0, 2)
	if userID != "" {
		keys = append(keys, freqCapKey(userID, campaignID))
	}
	if householdID != "" {
		keys = append(keys, freqCapKey(householdID, campaignID))
	}
	return keys
}

// evalInts runs a script and coerces the []any reply to int64s.
func (f *FreqCap) evalInts(ctx context.Context, script string, keys []string, args ...any) ([]int64, error) {
	sc, ok := f.l2.(cache.Scripter)
	if !ok {
		return nil, errNoScripting
	}
	raw, err := sc.Eval(ctx, script, keys, args...)
	if err != nil {
		return nil, err
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("freqcap script: unexpected reply %T", raw)
	}
	out := make([]int64, len(items))
	for i, it := range items {
		n, ok := it.(int64)
		if !ok {
			return nil, fmt.Errorf("freqcap script: unexpected element %T", it)
		}
		out[i] = n
	}
	return out, nil
}

var errNoScripting = errors.New("freqcap: backend does not support scripting")

// DecideAndRecord is the display path for both scopes in one round trip:
// check-and-increment user, then household only if the user allowed.
// blockedScope is "" (allowed), "user", or "household".
func (f *FreqCap) DecideAndRecord(ctx context.Context, userID, householdID, campaignID string, limit int, window time.Duration) (bool, string) {
	if limit <= 0 || (userID == "" && householdID == "") {
		return true, ""
	}
	// The script's user/household asymmetry needs a real user key; a
	// household-only request degrades to the single-scope serial call.
	if userID != "" {
		keys := capKeys(userID, householdID, campaignID)
		counts, err := f.evalInts(ctx, checkScript, keys, limit, window.Milliseconds())
		if err == nil {
			if counts[0] > int64(limit) {
				return false, "user"
			}
			if len(keys) > 1 && counts[1] > int64(limit) {
				return false, "household"
			}
			return true, ""
		}
		if err != errNoScripting {
			f.log.Warn("freqcap combined check failed; using serial path", "error", err)
		}
	}
	// Serial fallback — byte-for-byte the pre-Lua behaviour.
	if !f.AllowAndRecord(ctx, userID, campaignID, limit, window) {
		return false, "user"
	}
	if householdID != "" && !f.AllowAndRecord(ctx, householdID, campaignID, limit, window) {
		return false, "household"
	}
	return true, ""
}

// PeekBoth is the video/audio decision path: would the NEXT impression be
// allowed for both scopes, without incrementing either.
func (f *FreqCap) PeekBoth(ctx context.Context, userID, householdID, campaignID string, limit int) (bool, string) {
	if limit <= 0 || (userID == "" && householdID == "") {
		return true, ""
	}
	keys := capKeys(userID, householdID, campaignID)
	counts, err := f.evalInts(ctx, peekScript, keys)
	if err == nil {
		scopes := capScopes(userID, householdID)
		for i, c := range counts {
			if c >= int64(limit) {
				return false, scopes[i]
			}
		}
		return true, ""
	}
	if err != errNoScripting {
		f.log.Warn("freqcap combined peek failed; using serial path", "error", err)
	}
	if userID != "" && !f.Allow(ctx, userID, campaignID, limit) {
		return false, "user"
	}
	if householdID != "" && !f.Allow(ctx, householdID, campaignID, limit) {
		return false, "household"
	}
	return true, ""
}

// RecordBoth counts a confirmed impression against both scopes (stitch time).
func (f *FreqCap) RecordBoth(ctx context.Context, userID, householdID, campaignID string, limit int, window time.Duration) {
	if limit <= 0 || (userID == "" && householdID == "") {
		return
	}
	keys := capKeys(userID, householdID, campaignID)
	if sc, ok := f.l2.(cache.Scripter); ok {
		if _, err := sc.Eval(ctx, recordScript, keys, window.Milliseconds()); err == nil {
			return
		} else {
			f.log.Warn("freqcap combined record failed; using serial path", "error", err)
		}
	}
	f.Record(ctx, userID, campaignID, limit, window)
	f.Record(ctx, householdID, campaignID, limit, window)
}

// ── Campaign cap scope (per-campaign frequency_cap.scope knob) ────────────
//
// A campaign whose cap scope is "household" enforces its limit against the
// HOUSEHOLD counter (hh:… id + campaign key — the same key the default path's
// household leg already uses) INSTEAD of the per-user counter, so co-viewing
// devices share one allowance. This is a pure key swap in front of the
// existing combined FreqCap calls — the household id rides in the primary
// (user) slot and the secondary slot is dropped, so the Redis round-trip
// count is unchanged (hot-path iron rule). When no household id resolves the
// cap falls back to the per-user counter, mirroring how the platform
// household leg silently vanishes on an absent hh: id. Any other scope
// (empty/"user") keeps the pre-knob behaviour byte-for-byte: user counter
// primary, household counter co-enforced.

// capScopeIDs picks the counter ids for a campaign cap scope. hhPrimary
// reports that the household id took the primary slot (for blocked-scope
// attribution: the primitive reports the primary slot as "user").
func capScopeIDs(scope, userID, householdID string) (uid, hhid string, hhPrimary bool) {
	if scope == models.FreqCapScopeHousehold && householdID != "" {
		return householdID, "", true
	}
	return userID, householdID, false
}

// remapBlockedScope corrects the primitive's primary-slot attribution when the
// household id rode in the user slot.
func remapBlockedScope(blocked string, hhPrimary bool) string {
	if hhPrimary && blocked == "user" {
		return "household"
	}
	return blocked
}

// ScopedDecideAndRecord is DecideAndRecord with the campaign's cap scope
// applied (display path).
func (f *FreqCap) ScopedDecideAndRecord(ctx context.Context, scope, userID, householdID, campaignID string, limit int, window time.Duration) (bool, string) {
	uid, hhid, hhPrimary := capScopeIDs(scope, userID, householdID)
	ok, blocked := f.DecideAndRecord(ctx, uid, hhid, campaignID, limit, window)
	return ok, remapBlockedScope(blocked, hhPrimary)
}

// ScopedPeek is PeekBoth with the campaign's cap scope applied (video/audio
// serve decision — never increments).
func (f *FreqCap) ScopedPeek(ctx context.Context, scope, userID, householdID, campaignID string, limit int) (bool, string) {
	uid, hhid, hhPrimary := capScopeIDs(scope, userID, householdID)
	ok, blocked := f.PeekBoth(ctx, uid, hhid, campaignID, limit)
	return ok, remapBlockedScope(blocked, hhPrimary)
}

// ScopedRecord is RecordBoth with the campaign's cap scope applied (stitch-time
// counting — increments the same swapped key ScopedPeek decided on, keeping
// the PEEK/RECORD split consistent per scope).
func (f *FreqCap) ScopedRecord(ctx context.Context, scope, userID, householdID, campaignID string, limit int, window time.Duration) {
	uid, hhid, _ := capScopeIDs(scope, userID, householdID)
	f.RecordBoth(ctx, uid, hhid, campaignID, limit, window)
}

// capScopes mirrors capKeys' ordering for blocked-scope attribution.
func capScopes(userID, householdID string) []string {
	scopes := make([]string, 0, 2)
	if userID != "" {
		scopes = append(scopes, "user")
	}
	if householdID != "" {
		scopes = append(scopes, "household")
	}
	return scopes
}

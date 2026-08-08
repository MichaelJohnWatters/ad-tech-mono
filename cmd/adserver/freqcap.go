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

// ── Cap scope resolution (per-campaign frequency_cap.scope knob) ──────────
//
// The serve handler resolves the campaign's cap scope ONCE per request into
// an ordered list of enforcement legs — (scope label, counter id) pairs —
// and the combined ops below all consume that same resolution. One
// resolution driving peek, record, and check-and-record makes peek/record
// key mismatches unrepresentable, and blocked-scope attribution is intrinsic
// to the leg that blocked (no after-the-fact remapping).
//
// A campaign whose cap scope is "household" enforces its limit against the
// HOUSEHOLD counter (hh:… id + campaign key — the same key the default
// path's household leg already uses) INSTEAD of the per-user counter, so
// co-viewing devices share one allowance. The resolution only picks keys —
// the Redis round-trip count is unchanged (hot-path iron rule). When no
// household id resolves the cap falls back to the per-user counter,
// mirroring how the platform household leg silently vanishes on an absent
// hh: id. Any other scope (empty/"user") keeps the pre-knob behaviour
// byte-for-byte: user counter primary, household counter co-enforced.

// capLeg is one resolved enforcement leg: the id that owns the counter and
// the scope label a block on that counter is attributed to
// (models.FreqCapScopeUser | models.FreqCapScopeHousehold).
type capLeg struct {
	scope string
	id    string
}

// capResolution is the ordered (primary-first) set of enforcement legs for
// one serve request. Value type, at most two legs — nothing on the serve
// path allocates until a Redis script needs its key slice.
type capResolution struct {
	legs [2]capLeg
	n    int
}

func (r *capResolution) add(scope, id string) {
	r.legs[r.n] = capLeg{scope: scope, id: id}
	r.n++
}

// keys returns the Redis keys for the resolved legs, in leg order.
func (r capResolution) keys(campaignID string) []string {
	keys := make([]string, r.n)
	for i := 0; i < r.n; i++ {
		keys[i] = freqCapKey(r.legs[i].id, campaignID)
	}
	return keys
}

// resolveCapScope maps (campaign cap scope, user id, household id) to the
// enforcement legs. Deterministic, so the separate peek and record requests
// of a video/audio PEEK/RECORD split resolve to identical keys. Absent ids
// contribute no leg (an empty resolution bypasses the cap, matching the
// primitives' empty-id bypass).
func resolveCapScope(scope, userID, householdID string) capResolution {
	var r capResolution
	if scope == models.FreqCapScopeHousehold && householdID != "" {
		r.add(models.FreqCapScopeHousehold, householdID)
		return r
	}
	if userID != "" {
		r.add(models.FreqCapScopeUser, userID)
	}
	if householdID != "" {
		r.add(models.FreqCapScopeHousehold, householdID)
	}
	return r
}

// ── Combined multi-leg ops: ONE Redis round trip via Lua ──────────────────
//
// The serve handler runs the cap for up to two legs on every request; as
// four serial ops (2× INCR + 2× EXPIRE) that was the whole render leg's cost
// under load (adserver freqcap phase p95 80ms vs 1-2ms for everything else,
// 2026-08-05). Each script preserves the serial path's exact semantics —
// notably check-and-record only touches the secondary (household) counter
// when the primary leg ALLOWED (the pre-existing asymmetry). When the
// backend can't script (in-memory fallback era, tests, Redis blip) every
// method degrades to the original per-leg calls.

// checkScript: INCR the primary leg (PEXPIRE on first); if over limit stop
// (secondary untouched). Else INCR the secondary when present. Returns
// {primaryCount, secondaryCount}; secondaryCount -1 = not evaluated
// (blocked at primary or no secondary key).
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

// DecideAndRecord is the display path for the resolved legs in one round
// trip: check-and-increment the primary leg, then the secondary only if the
// primary allowed. blockedScope is "" (allowed) or the blocking leg's scope
// label ("user" / "household") — attribution comes straight from the
// resolution, so it is correct in every scope mode by construction.
func (f *FreqCap) DecideAndRecord(ctx context.Context, res capResolution, campaignID string, limit int, window time.Duration) (bool, string) {
	if limit <= 0 || res.n == 0 {
		return true, ""
	}
	counts, err := f.evalInts(ctx, checkScript, res.keys(campaignID), limit, window.Milliseconds())
	if err == nil {
		if counts[0] > int64(limit) {
			return false, res.legs[0].scope
		}
		if res.n > 1 && counts[1] > int64(limit) {
			return false, res.legs[1].scope
		}
		return true, ""
	}
	if err != errNoScripting {
		f.log.Warn("freqcap combined check failed; using serial path", "error", err)
	}
	// Serial fallback — byte-for-byte the pre-Lua behaviour: stop at the
	// first blocking leg, later legs untouched.
	for i := 0; i < res.n; i++ {
		if !f.AllowAndRecord(ctx, res.legs[i].id, campaignID, limit, window) {
			return false, res.legs[i].scope
		}
	}
	return true, ""
}

// Peek is the video/audio decision path: would the NEXT impression be
// allowed for every resolved leg, without incrementing any counter.
func (f *FreqCap) Peek(ctx context.Context, res capResolution, campaignID string, limit int) (bool, string) {
	if limit <= 0 || res.n == 0 {
		return true, ""
	}
	counts, err := f.evalInts(ctx, peekScript, res.keys(campaignID))
	if err == nil {
		for i, c := range counts {
			if c >= int64(limit) {
				return false, res.legs[i].scope
			}
		}
		return true, ""
	}
	if err != errNoScripting {
		f.log.Warn("freqcap combined peek failed; using serial path", "error", err)
	}
	for i := 0; i < res.n; i++ {
		if !f.Allow(ctx, res.legs[i].id, campaignID, limit) {
			return false, res.legs[i].scope
		}
	}
	return true, ""
}

// RecordAll counts a confirmed impression against every resolved leg
// (stitch time). The record request re-resolves from the same (scope, user,
// household) inputs the peek did, so it increments exactly the keys the
// peek decided on — the PEEK/RECORD split stays consistent per scope mode.
func (f *FreqCap) RecordAll(ctx context.Context, res capResolution, campaignID string, limit int, window time.Duration) {
	if limit <= 0 || res.n == 0 {
		return
	}
	if sc, ok := f.l2.(cache.Scripter); ok {
		if _, err := sc.Eval(ctx, recordScript, res.keys(campaignID), window.Milliseconds()); err == nil {
			return
		} else {
			f.log.Warn("freqcap combined record failed; using serial path", "error", err)
		}
	}
	for i := 0; i < res.n; i++ {
		f.Record(ctx, res.legs[i].id, campaignID, limit, window)
	}
}

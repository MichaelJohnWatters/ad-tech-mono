package main

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	audstore "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// identityResolver expands a user key to the identifiers linked to it in the
// identity graph. Satisfied by *postgres.Store (ResolveIdentity).
type identityResolver interface {
	ResolveIdentity(ctx context.Context, id string) ([]string, error)
}

// openIdentityResolver builds the DSP's identity resolver when
// dsp.identity_resolution_enabled is set. Off by default: identity resolution
// adds a Postgres lookup to the bid path, so it's opt-in. Returns (nil, no-op)
// when disabled or Postgres is unreachable.
func openIdentityResolver(cfg *config.Config, log *slog.Logger) (identityResolver, func()) {
	if !cfg.GetBool("dsp.identity_resolution_enabled", false) {
		return nil, func() {}
	}
	dbURL := cfg.Get("database.url", "")
	if dbURL == "" {
		log.Warn("dsp identity resolution enabled but database.url unset; resolution disabled")
		return nil, func() {}
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Warn("dsp identity resolver open failed", "error", err)
		return nil, func() {}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Warn("dsp identity resolver ping failed", "error", err)
		_ = db.Close()
		return nil, func() {}
	}
	log.Info("dsp identity resolution enabled")
	return postgres.NewFromDB(db), func() { _ = db.Close() }
}

// dspPrivateSegments returns the DSP-private segment ids for a user. When a
// resolver is present, the user is first expanded via the identity graph so
// segments attached to a linked identifier (from another device / publisher /
// a UID2 CRM match) also apply — this is what makes UID2 addressable beyond a
// single request. Best-effort throughout: a failed resolve or per-id lookup is
// skipped so the bid still proceeds; the whole thing runs under the caller's
// (tight) deadline. maxLinked caps how many linked ids are folded in.
func dspPrivateSegments(ctx context.Context, store audstore.Lookup, resolver identityResolver, userKey string, maxLinked int, log *slog.Logger) []string {
	if store == nil || userKey == "" {
		return nil
	}
	ids := []string{userKey}
	if resolver != nil {
		linked, err := resolver.ResolveIdentity(ctx, userKey)
		if err != nil {
			log.Debug("identity resolve degraded (bid proceeds without linked segments)", "user_key", userKey, "error", err)
		} else {
			for _, id := range linked {
				if len(ids) > maxLinked { // ids already holds userKey, so this caps linked adds
					break
				}
				ids = append(ids, id)
			}
		}
	}
	seen := make(map[string]bool)
	var out []string
	for _, id := range ids {
		segs, err := store.DSPSegmentsForUser(ctx, id)
		if err != nil {
			log.Debug("dsp private segment lookup degraded", "id", id, "error", err)
			continue
		}
		for _, s := range segs {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

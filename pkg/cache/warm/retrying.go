package warm

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

// RetryingLoader wraps a constructor that may fail (typically because the
// downstream dependency — Postgres, gRPC backend, etc. — is unavailable)
// and turns the failure into a transparent retry. On each LoadAll:
//
//  1. If the inner loader hasn't been constructed yet, call Construct.
//     If Construct fails, log and return (nil, nil) — the cache keeps
//     serving the previous snapshot. The next poll tick tries again.
//  2. If the inner loader exists but its LoadAll returns an error,
//     drop the inner loader so the next call retries Construct (the
//     existing handle may be tied to a dead connection).
//
// Why this matters: every cmd/<svc> service previously called pickXxxLoader
// at boot, and if Postgres was unreachable in that moment it pinned an
// empty-loader fallback for the process lifetime. Wrapping with this
// makes the loader self-healing: any service that booted before
// Postgres was ready will reconnect on the next 30s poll, no operator
// restart needed. This was the recurring root cause of every "the pub
// sim shows 0 deals / 1 creative / 1 contract" report.
//
// KeyOf must be supplied separately because we may not have an inner
// loader yet to delegate to.
var errNoSingleLoader = errors.New("inner loader has no LoadOne")

type RetryingLoader[T any] struct {
	Construct func() (Loader[T], error)
	KeyFn     func(T) string
	Log       *slog.Logger

	mu    sync.Mutex
	inner Loader[T]
}

func (r *RetryingLoader[T]) LoadAll(ctx context.Context) ([]T, error) {
	r.mu.Lock()
	if r.inner == nil {
		inner, err := r.Construct()
		if err != nil {
			r.mu.Unlock()
			if r.Log != nil {
				r.Log.Warn("retrying loader: construct failed, will retry on next poll", "error", err)
			}
			return nil, nil
		}
		r.inner = inner
	}
	inner := r.inner
	r.mu.Unlock()

	rows, err := inner.LoadAll(ctx)
	if err != nil {
		// Connection may have died — drop the inner so the next call
		// re-constructs against fresh state.
		r.mu.Lock()
		r.inner = nil
		r.mu.Unlock()
		if r.Log != nil {
			r.Log.Warn("retrying loader: inner LoadAll failed, will reconstruct on next poll", "error", err)
		}
		return nil, nil
	}
	return rows, nil
}

func (r *RetryingLoader[T]) KeyOf(t T) string {
	if r.KeyFn != nil {
		return r.KeyFn(t)
	}
	return ""
}

// LoadOne forwards to the inner loader's single-fetch when it has one, so a
// RetryingLoader-wrapped SingleLoader (e.g. the DSP campaign cache) keeps its
// targeted-invalidation fast path instead of falling back to a full reload.
// Constructs the inner on demand like LoadAll; a construct/inner failure
// returns an error so the caller reloads fully (the safe default).
func (r *RetryingLoader[T]) LoadOne(ctx context.Context, key string) (T, bool, error) {
	var zero T
	r.mu.Lock()
	if r.inner == nil {
		inner, err := r.Construct()
		if err != nil {
			r.mu.Unlock()
			return zero, false, err
		}
		r.inner = inner
	}
	inner := r.inner
	r.mu.Unlock()

	sl, ok := inner.(SingleLoader[T])
	if !ok {
		return zero, false, errNoSingleLoader
	}
	v, found, err := sl.LoadOne(ctx, key)
	if err != nil {
		// Same drop-and-reconstruct posture as LoadAll on a dead connection.
		r.mu.Lock()
		r.inner = nil
		r.mu.Unlock()
	}
	return v, found, err
}

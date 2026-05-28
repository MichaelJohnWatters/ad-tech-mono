package postgres_test

import (
	"context"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

func TestWithAccountID(t *testing.T) {
	ctx := context.Background()
	ctx = postgres.WithAccountID(ctx, "acc-123")

	got := postgres.AccountIDFromContext(ctx)
	if got != "acc-123" {
		t.Errorf("AccountIDFromContext() = %q, want 'acc-123'", got)
	}
}

func TestAccountIDFromContext_Empty(t *testing.T) {
	ctx := context.Background()
	got := postgres.AccountIDFromContext(ctx)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := postgres.DefaultConfig()
	if cfg.PrimaryURL == "" {
		t.Error("PrimaryURL should not be empty")
	}
	if cfg.MaxOpenConns <= 0 {
		t.Error("MaxOpenConns should be positive")
	}
}

// Integration tests (require real Postgres) are tagged with:
//   go test -tags=integration ./pkg/store/postgres/...
// They use testcontainers-go to spin up a real Postgres instance.
// See Layer 2 testing strategy in docs/PLAN.md.

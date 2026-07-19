//go:build integration

package houseads

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	_ "github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// TestStoreCRUD runs the house-ad store against a real Postgres (never mocked,
// per repo convention). Uses DATABASE_URL or the local stack default; skips when
// Postgres is unreachable or migration 053 has not been applied.
func TestStoreCRUD(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = routes.DefaultPostgresURL
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Skipf("postgres open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("postgres unreachable (%v) — start the local stack or set DATABASE_URL", err)
	}
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'house_ads')`,
	).Scan(&exists); err != nil || !exists {
		t.Skipf("house_ads table missing (migration 053 not applied): %v", err)
	}

	store := NewStore(db)

	// Create.
	id, err := store.Create(ctx, Input{
		Format: FormatVideo, Name: "House Video", Markup: "<VAST/>",
		LandingURL: "https://adtech.example/house", Enabled: true, Weight: 2,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM house_ads WHERE id = $1::uuid`, id) })

	// Get round-trips every field.
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Format != FormatVideo || got.Name != "House Video" || got.Markup != "<VAST/>" ||
		got.LandingURL != "https://adtech.example/house" || !got.Enabled || got.Weight != 2 {
		t.Fatalf("get mismatch: %+v", got)
	}

	// List includes it.
	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, h := range all {
		if h.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("list did not include created house ad %s", id)
	}

	// Update replaces the mutable fields.
	if err := store.Update(ctx, id, Input{
		Format: FormatAudio, Name: "House Audio", Markup: "<VAST2/>", Enabled: false, Weight: 5,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = store.Get(ctx, id)
	if got.Format != FormatAudio || got.Name != "House Audio" || got.Enabled || got.Weight != 5 || got.LandingURL != "" {
		t.Fatalf("post-update mismatch: %+v", got)
	}

	// Update of an unknown id → sql.ErrNoRows.
	if err := store.Update(ctx, "00000000-0000-4000-8000-000000000000", Input{Format: FormatVideo, Name: "x", Markup: "y"}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("update unknown: err = %v, want sql.ErrNoRows", err)
	}

	// Delete removes it; a second delete → sql.ErrNoRows.
	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.Delete(ctx, id); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("re-delete: err = %v, want sql.ErrNoRows", err)
	}
}

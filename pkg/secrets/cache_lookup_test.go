package secrets

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache/warm"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

type fakeSecretLoader struct{ secrets []Secret }

func (f *fakeSecretLoader) LoadAll(context.Context) ([]Secret, error) { return f.secrets, nil }
func (f *fakeSecretLoader) KeyOf(s Secret) string                     { return s.ID }

func testCache(t *testing.T, secs []Secret) *Cache {
	t.Helper()
	inner := warm.New(warm.Config[Secret]{
		Name:         "secrets",
		Loader:       &fakeSecretLoader{secrets: secs},
		Clock:        clock.Real{},
		PollInterval: time.Hour,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := inner.Start(context.Background()); err != nil {
		t.Fatalf("cache start: %v", err)
	}
	t.Cleanup(inner.Stop)
	return &Cache{Cache: inner}
}

func TestLookupActiveByPurpose(t *testing.T) {
	c := testCache(t, []Secret{
		{ID: "1", Name: "old-jwt", Purpose: PurposeJWTSigning, Status: StatusRotating, Value: "old"},
		{ID: "2", Name: "jwt", Purpose: PurposeJWTSigning, Status: StatusActive, Value: "signing-key"},
		{ID: "3", Name: "hmac", Purpose: PurposeHMACTracker, Status: StatusActive, Value: "hmac"},
	})

	sec, ok := c.LookupActiveByPurpose(PurposeJWTSigning)
	if !ok {
		t.Fatal("expected an active jwt_signing secret")
	}
	if sec.Value != "signing-key" {
		t.Errorf("got value %q, want the active one (not the rotating predecessor)", sec.Value)
	}

	if _, ok := c.LookupActiveByPurpose(PurposeAPIKey); ok {
		t.Error("expected no active api_key secret")
	}
}

func TestLookupActiveByPurpose_NoActive(t *testing.T) {
	// Only a rotating key exists — not acceptable as the canonical signer.
	c := testCache(t, []Secret{
		{ID: "1", Name: "jwt", Purpose: PurposeJWTSigning, Status: StatusRotating, Value: "old"},
	})
	if _, ok := c.LookupActiveByPurpose(PurposeJWTSigning); ok {
		t.Error("rotating-only should not be returned as active")
	}
}

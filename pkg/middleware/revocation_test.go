package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/cache"
)

func TestRevocationStore_RevokeThenCheck(t *testing.T) {
	s := NewRevocationStore(cache.NewMemoryL2(), 12*time.Hour, nil)
	ctx := context.Background()
	now := time.Now()

	// A token issued an hour ago, before any revoke → not revoked.
	old := now.Add(-time.Hour)
	if revoked, _ := s.IsRevoked(ctx, "user-1", old); revoked {
		t.Fatal("token should not be revoked before any RevokeUser")
	}

	// Revoke now → the old token is killed, a token issued later survives.
	if err := s.RevokeUser(ctx, "user-1", now); err != nil {
		t.Fatalf("RevokeUser: %v", err)
	}
	if revoked, _ := s.IsRevoked(ctx, "user-1", old); !revoked {
		t.Error("token issued before the revoke should be revoked")
	}
	future := now.Add(time.Second)
	if revoked, _ := s.IsRevoked(ctx, "user-1", future); revoked {
		t.Error("token issued after the revoke should survive (fresh login)")
	}
	// A different user is unaffected.
	if revoked, _ := s.IsRevoked(ctx, "user-2", old); revoked {
		t.Error("revoke must be scoped to the target user")
	}
}

func TestAuth_RejectsRevokedToken(t *testing.T) {
	const key = "test-signing-key-32bytes-long!!"
	s := NewRevocationStore(cache.NewMemoryL2(), 12*time.Hour, nil)
	now := time.Now()
	claims := &auth.Claims{
		UserID:    "user-42",
		AccountID: "acc-1",
		IssuedAt:  now.Add(-time.Minute),
		ExpiresAt: now.Add(time.Hour),
	}
	token, err := CreateToken(claims, key)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	h := Auth(key, quietMWLog(), WithRevocation(s))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	call := func() int {
		r := httptest.NewRequest("GET", "/v1/api/x", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	if got := call(); got != http.StatusOK {
		t.Fatalf("before revoke: got %d, want 200", got)
	}
	if err := s.RevokeUser(context.Background(), "user-42", time.Now()); err != nil {
		t.Fatalf("RevokeUser: %v", err)
	}
	if got := call(); got != http.StatusUnauthorized {
		t.Errorf("after revoke: got %d, want 401", got)
	}
}

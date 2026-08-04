package auth_test

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

func TestHasPermission(t *testing.T) {
	claims := &auth.Claims{
		Permissions: []string{"campaigns:create", "campaigns:read", "billing:view"},
	}

	if !auth.HasPermission(claims, "campaigns:create") {
		t.Error("should have campaigns:create")
	}
	if auth.HasPermission(claims, "campaigns:delete") {
		t.Error("should not have campaigns:delete")
	}
}

func TestHasAnyPermission(t *testing.T) {
	claims := &auth.Claims{
		Permissions: []string{"reports:read"},
	}

	if !auth.HasAnyPermission(claims, "campaigns:read", "reports:read") {
		t.Error("should have at least one")
	}
	if auth.HasAnyPermission(claims, "campaigns:delete", "settings:update") {
		t.Error("should not have any")
	}
}

func TestCanAccessAccount_OwnAccount(t *testing.T) {
	claims := &auth.Claims{
		AccountID:   "adv_123",
		AccountType: auth.AccountAdvertiser,
	}

	if !auth.CanAccessAccount(claims, "adv_123") {
		t.Error("advertiser should access own account")
	}
	if auth.CanAccessAccount(claims, "adv_456") {
		t.Error("advertiser should NOT access other account")
	}
}

func TestCanAccessAccount_Agency(t *testing.T) {
	claims := &auth.Claims{
		AccountID:       "agency_1",
		AccountType:     auth.AccountAgency,
		ManagedAccounts: []string{"adv_123", "adv_456"},
	}

	if !auth.CanAccessAccount(claims, "agency_1") {
		t.Error("agency should access own account")
	}
	if !auth.CanAccessAccount(claims, "adv_123") {
		t.Error("agency should access managed account")
	}
	if auth.CanAccessAccount(claims, "adv_789") {
		t.Error("agency should NOT access unmanaged account")
	}
}

func TestCanAccessAccount_Staff(t *testing.T) {
	claims := &auth.Claims{
		AccountID:   "staff_1",
		AccountType: auth.AccountStaff,
	}

	if !auth.CanAccessAccount(claims, "adv_123") {
		t.Error("staff should access any account")
	}
	if !auth.CanAccessAccount(claims, "pub_456") {
		t.Error("staff should access any account")
	}
}

func TestCanAccessAccount_Admin(t *testing.T) {
	claims := &auth.Claims{
		AccountID:   "admin_1",
		AccountType: auth.AccountAdmin,
	}

	if !auth.CanAccessAccount(claims, "any_account") {
		t.Error("admin should access any account")
	}
}

func TestIsPlatformUser(t *testing.T) {
	staffClaims := &auth.Claims{AccountType: auth.AccountStaff}
	adminClaims := &auth.Claims{AccountType: auth.AccountAdmin}
	advClaims := &auth.Claims{AccountType: auth.AccountAdvertiser}

	if !auth.IsPlatformUser(staffClaims) {
		t.Error("staff should be platform user")
	}
	if !auth.IsPlatformUser(adminClaims) {
		t.Error("admin should be platform user")
	}
	if auth.IsPlatformUser(advClaims) {
		t.Error("advertiser should NOT be platform user")
	}
}

func TestIsExpired(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC))

	claims := &auth.Claims{
		ExpiresAt: time.Date(2026, 5, 28, 13, 0, 0, 0, time.UTC), // 1 hour from now
	}

	if claims.IsExpired(clk) {
		t.Error("should not be expired yet")
	}

	clk.Advance(2 * time.Hour)

	if !claims.IsExpired(clk) {
		t.Error("should be expired after 2 hours")
	}
}

func TestRolePermissions(t *testing.T) {
	perms := auth.RolePermissions(auth.AccountAdvertiser, auth.RoleOwner)
	if len(perms) == 0 {
		t.Error("advertiser owner should have permissions")
	}

	// Check specific permission exists
	has := false
	for _, p := range perms {
		if p == "campaigns:create" {
			has = true
			break
		}
	}
	if !has {
		t.Error("advertiser owner should have campaigns:create")
	}

	// Viewer should not have create
	viewerPerms := auth.RolePermissions(auth.AccountAdvertiser, auth.RoleViewer)
	for _, p := range viewerPerms {
		if p == "campaigns:create" {
			t.Error("viewer should NOT have campaigns:create")
		}
	}

	// Unknown role
	unknown := auth.RolePermissions("unknown", "unknown")
	if unknown != nil {
		t.Error("unknown role should return nil")
	}
}

// TestUserIDMintAndRecover locks the JWT-subject prefix convention: MintUserID
// namespaces a team_members id and TeamMemberID recovers the bare id (so callers
// never cast a "user-<id>" subject straight to ::uuid — the 22P02 bug that
// silently killed the ingest-completion email).
func TestUserIDMintAndRecover(t *testing.T) {
	const id = "fcb4d101-6ddd-475a-97bb-d89683cf0812"
	minted := auth.MintUserID(id)
	if minted != "user-"+id {
		t.Fatalf("MintUserID = %q, want %q", minted, "user-"+id)
	}
	if got := auth.TeamMemberID(minted); got != id {
		t.Errorf("TeamMemberID(%q) = %q, want %q", minted, got, id)
	}
	// Idempotent / tolerant: a bare id (or padded) round-trips unchanged.
	if got := auth.TeamMemberID(id); got != id {
		t.Errorf("TeamMemberID(bare) = %q, want %q", got, id)
	}
	if got := auth.TeamMemberID("  " + minted + "  "); got != id {
		t.Errorf("TeamMemberID(padded) = %q, want %q", got, id)
	}
}

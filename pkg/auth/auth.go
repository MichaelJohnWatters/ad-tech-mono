// Package auth provides JWT-based authentication and RBAC
// for the platform's 5 account types and role hierarchy.
//
// Account types: advertiser, publisher, agency, staff, admin
// Permissions: resource:action format (e.g. "campaigns:create")
//
// Usage:
//
//	claims, err := auth.ValidateToken(tokenString, signingKey)
//	if err != nil { // invalid or expired }
//	if !auth.HasPermission(claims, "campaigns:create") { // forbidden }
package auth

import (
	"fmt"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

// AccountType represents the type of account.
type AccountType string

const (
	AccountAdvertiser AccountType = "advertiser"
	AccountPublisher  AccountType = "publisher"
	AccountAgency     AccountType = "agency"
	AccountStaff      AccountType = "staff"
	AccountAdmin      AccountType = "admin"
)

// Role represents a user's role within their account type.
type Role string

const (
	RoleOwner   Role = "owner"
	RoleManager Role = "manager"
	RoleAnalyst Role = "analyst"
	RoleFinance Role = "finance"
	RoleViewer  Role = "viewer"
	RoleAdOps   Role = "ad_ops"
)

// Claims represents the decoded JWT payload.
type Claims struct {
	UserID          string      `json:"sub"`
	AccountID       string      `json:"account_id"`
	AccountType     AccountType `json:"account_type"`
	Role            Role        `json:"role"`
	Permissions     []string    `json:"permissions"`
	ManagedAccounts []string    `json:"managed_accounts"` // agency only
	IssuedAt        time.Time   `json:"iat"`
	ExpiresAt       time.Time   `json:"exp"`
}

// IsExpired checks if the token has expired using the provided clock.
func (c *Claims) IsExpired(clk clock.Clock) bool {
	return clk.Now().After(c.ExpiresAt)
}

// HasPermission checks if the claims include the given permission.
func HasPermission(claims *Claims, permission string) bool {
	for _, p := range claims.Permissions {
		if p == permission {
			return true
		}
	}
	return false
}

// HasAnyPermission checks if the claims include any of the given permissions.
func HasAnyPermission(claims *Claims, permissions ...string) bool {
	for _, p := range permissions {
		if HasPermission(claims, p) {
			return true
		}
	}
	return false
}

// CanAccessAccount checks if the user can access data for the given account.
// Advertisers/publishers can only access their own account.
// Agencies can access their managed accounts.
// Staff/admin can access any account.
func CanAccessAccount(claims *Claims, targetAccountID string) bool {
	switch claims.AccountType {
	case AccountAdmin, AccountStaff:
		return true // platform staff can access any account
	case AccountAgency:
		if claims.AccountID == targetAccountID {
			return true
		}
		for _, managed := range claims.ManagedAccounts {
			if managed == targetAccountID {
				return true
			}
		}
		return false
	default:
		return claims.AccountID == targetAccountID
	}
}

// IsPlatformUser checks if the user is staff or admin.
func IsPlatformUser(claims *Claims) bool {
	return claims.AccountType == AccountStaff || claims.AccountType == AccountAdmin
}

// RolePermissions returns the default permissions for a given account type and role.
func RolePermissions(accountType AccountType, role Role) []string {
	key := fmt.Sprintf("%s:%s", accountType, role)
	if perms, ok := defaultPermissions[key]; ok {
		return perms
	}
	return nil
}

// defaultPermissions maps account_type:role to their default permission set.
var defaultPermissions = map[string][]string{
	// Advertiser roles
	"advertiser:owner": {
		"campaigns:create", "campaigns:read", "campaigns:update", "campaigns:delete",
		"campaigns:submit", "campaigns:pause", "campaigns:resume",
		"creatives:upload", "creatives:read", "creatives:update", "creatives:delete",
		"audiences:create", "audiences:read", "audiences:update", "audiences:delete", "audiences:upload",
		"billing:view", "billing:topup", "billing:dispute",
		"reports:read", "reports:export", "reports:save",
		"team:read", "team:invite", "team:update", "team:remove",
		"settings:read", "settings:update",
		"webhooks:read", "webhooks:create", "webhooks:update", "webhooks:delete",
	},
	"advertiser:manager": {
		"campaigns:create", "campaigns:read", "campaigns:update",
		"campaigns:submit", "campaigns:pause", "campaigns:resume",
		"creatives:upload", "creatives:read", "creatives:update",
		"audiences:create", "audiences:read", "audiences:upload",
		"billing:view",
		"reports:read", "reports:export", "reports:save",
		"team:read", "team:invite",
		"settings:read",
	},
	"advertiser:analyst": {
		"campaigns:read",
		"creatives:read",
		"audiences:read",
		"billing:view",
		"reports:read", "reports:export", "reports:save",
	},
	"advertiser:finance": {
		"billing:view", "billing:topup", "billing:dispute",
		"reports:read",
	},
	"advertiser:viewer": {
		"campaigns:read",
		"creatives:read",
		"billing:view",
		"reports:read",
	},

	// Publisher roles
	"publisher:owner": {
		"placements:create", "placements:read", "placements:update", "placements:delete",
		"deals:create", "deals:read", "deals:update", "deals:delete",
		"quality:read", "quality:update",
		"earnings:view",
		"reports:read", "reports:export", "reports:save",
		"pipeline:upload", "pipeline:read",
		"team:read", "team:invite", "team:update", "team:remove",
		"settings:read", "settings:update",
		"webhooks:read", "webhooks:create", "webhooks:update", "webhooks:delete",
	},
	"publisher:manager": {
		"placements:create", "placements:read", "placements:update",
		"deals:create", "deals:read", "deals:update",
		"quality:read", "quality:update",
		"earnings:view",
		"reports:read", "reports:export",
		"pipeline:upload",
		"team:read", "team:invite",
		"settings:read",
	},
	"publisher:ad_ops": {
		"placements:read", "placements:update",
		"quality:read", "quality:update",
		"reports:read",
	},
	"publisher:analyst": {
		"placements:read",
		"deals:read",
		"earnings:view",
		"reports:read", "reports:export", "reports:save",
		"pipeline:read",
	},
	"publisher:finance": {
		"earnings:view",
		"reports:read",
	},
	"publisher:viewer": {
		"placements:read",
		"deals:read",
		"earnings:view",
		"reports:read",
	},

	// Platform roles
	"admin:owner": {"*"}, // full access
	"staff:owner": {
		"moderation:read", "moderation:approve", "moderation:reject",
		"fraud:read", "fraud:update",
		"support:read", "support:update",
		"config:read", "config:update",
		"ops:read", "ops:deploy", "ops:ab_test",
		"reports:read", "reports:export",
		"audit:read",
	},
}

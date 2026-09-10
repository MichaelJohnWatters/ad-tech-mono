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
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/clock"
)

// userIDPrefix namespaces the JWT subject (Claims.UserID) for human-typed users,
// e.g. "user-<team_members.id>". Mint with MintUserID and read the bare id back
// with TeamMemberID — never inline the prefix, and never cast Claims.UserID to a
// uuid directly (that throws 22P02: the prefix makes it a non-uuid string).
const userIDPrefix = "user-"

// MintUserID builds a JWT subject (Claims.UserID) from a team_members id.
func MintUserID(teamMemberID string) string { return userIDPrefix + teamMemberID }

// TeamMemberID recovers the bare team_members id from a JWT subject, stripping the
// MintUserID prefix so the value can be matched against team_members.id (::uuid).
// Prefix-free input is returned unchanged.
func TeamMemberID(jwtUserID string) string {
	return strings.TrimPrefix(strings.TrimSpace(jwtUserID), userIDPrefix)
}

// AccountType represents the type of account.
type AccountType string

const (
	AccountAdvertiser AccountType = "advertiser"
	AccountPublisher  AccountType = "publisher"
	AccountAgency     AccountType = "agency"
	AccountStaff      AccountType = "staff"
	AccountAdmin      AccountType = "admin"
	// AccountPartner is an external integration partner (a demand DSP or supply
	// SSP) with a self-serve portal to manage its sandbox integration. Registered
	// + provisioned by staff (PLAN Phase 11 #112); minimal permissions.
	AccountPartner AccountType = "partner"
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
	RoleDevOps  Role = "devops"
)

// Claims represents the decoded JWT payload.
type Claims struct {
	UserID          string      `json:"sub"`
	AccountID       string      `json:"account_id"`
	AccountType     AccountType `json:"account_type"`
	Role            Role        `json:"role"`
	Permissions     []string    `json:"permissions"`
	ManagedAccounts []string    `json:"managed_accounts"` // agency only
	// ResidencyRegion is the account's data-residency region (accounts.residency_region),
	// carried so the gateway's region gate can reject out-of-region mutations without a
	// per-request DB lookup. Empty = unpinned (treated as the home region). Stale until
	// the next login if an operator changes it.
	ResidencyRegion string    `json:"residency_region,omitempty"`
	IssuedAt        time.Time `json:"iat"`
	ExpiresAt       time.Time `json:"exp"`
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
		"marketplace:read", "marketplace:list", "marketplace:buy",
		"billing:view", "billing:topup", "billing:dispute",
		"reports:read", "reports:export", "reports:save",
		"team:read", "team:invite", "team:update", "team:remove",
		"settings:read", "settings:update",
		"account:export", "account:close",
		"support:contact",
		"webhooks:read", "webhooks:create", "webhooks:update", "webhooks:delete",
	},
	"advertiser:manager": {
		"campaigns:create", "campaigns:read", "campaigns:update",
		"campaigns:submit", "campaigns:pause", "campaigns:resume",
		"creatives:upload", "creatives:read", "creatives:update",
		"audiences:create", "audiences:read", "audiences:upload",
		"marketplace:read", "marketplace:buy",
		"billing:view",
		"reports:read", "reports:export", "reports:save",
		"team:read", "team:invite",
		"settings:read",
		"support:contact",
	},
	"advertiser:analyst": {
		"campaigns:read",
		"creatives:read",
		"audiences:read",
		"marketplace:read",
		"billing:view",
		"reports:read", "reports:export", "reports:save",
	},
	"advertiser:finance": {
		"billing:view", "billing:topup", "billing:dispute",
		"reports:read",
		"support:contact",
	},
	"advertiser:viewer": {
		"campaigns:read",
		"creatives:read",
		"billing:view",
		"reports:read",
	},

	// Publisher roles. Audiences: publishers onboard their first-party user
	// lists (subscriber segments, content-affinity cohorts) the same way
	// advertisers onboard CRM lists — the supply-side half of the profile
	// store's onboarding surface.
	"publisher:owner": {
		"placements:create", "placements:read", "placements:update", "placements:delete",
		"deals:create", "deals:read", "deals:update", "deals:delete",
		"quality:read", "quality:update",
		"audiences:create", "audiences:read", "audiences:update", "audiences:delete", "audiences:upload",
		"marketplace:read", "marketplace:list",
		"earnings:view", "earnings:manage",
		"reports:read", "reports:export", "reports:save",
		"pipeline:upload", "pipeline:read",
		"team:read", "team:invite", "team:update", "team:remove",
		"settings:read", "settings:update",
		"account:export", "account:close",
		"support:contact",
		"webhooks:read", "webhooks:create", "webhooks:update", "webhooks:delete",
	},
	"publisher:manager": {
		"placements:create", "placements:read", "placements:update",
		"deals:create", "deals:read", "deals:update",
		"quality:read", "quality:update",
		"audiences:create", "audiences:read", "audiences:upload",
		"marketplace:read", "marketplace:list",
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
		"audiences:read",
		"earnings:view",
		"reports:read", "reports:export", "reports:save",
		"pipeline:read",
	},
	"publisher:finance": {
		"earnings:view", "earnings:manage",
		"reports:read",
	},
	"publisher:viewer": {
		"placements:read",
		"deals:read",
		"earnings:view",
		"reports:read",
	},

	// Agency roles — a media-buying agency acting on behalf of managed
	// advertiser accounts. Same buy-side surface as an advertiser owner; the
	// act-as target (a managed account) selects which tenant the request scopes
	// to. "agency:read" gates the managed-account list + switcher.
	"agency:owner": {
		"campaigns:create", "campaigns:read", "campaigns:update", "campaigns:delete",
		"campaigns:submit", "campaigns:pause", "campaigns:resume",
		"creatives:upload", "creatives:read", "creatives:update", "creatives:delete",
		"audiences:create", "audiences:read", "audiences:update", "audiences:upload",
		"billing:view",
		"reports:read", "reports:export", "reports:save",
		"team:read", "team:invite", "team:update", "team:remove",
		"settings:read", "settings:update",
		"agency:read",
	},
	"agency:manager": {
		"campaigns:create", "campaigns:read", "campaigns:update",
		"campaigns:submit", "campaigns:pause", "campaigns:resume",
		"creatives:upload", "creatives:read", "creatives:update",
		"reports:read", "reports:export",
		"agency:read",
	},
	"agency:analyst": {
		"campaigns:read", "creatives:read",
		"reports:read", "reports:export",
		"agency:read",
	},

	// Partner roles — an external integration partner's self-serve portal. Minimal
	// surface: view their own onboarding record, manage sandbox API keys, read the
	// integration guide, contact support. NO buy/sell/report access.
	"partner:owner": {
		"partner:self",
		"apikeys:manage",
		"settings:read", "settings:update",
		"support:contact",
	},
	"partner:manager": {
		"partner:self",
		"apikeys:manage",
		"support:contact",
	},

	// Platform roles
	"admin:owner": {"*"}, // full access
	"staff:owner": {
		"moderation:read", "moderation:approve", "moderation:reject",
		"fraud:read", "fraud:update",
		"support:read", "support:update",
		"config:read", "config:update",
		"ops:read", "ops:deploy", "ops:ab_test",
		"incidents:read", "incidents:write",
		"changelog:read", "changelog:write",
		"partners:read", "partners:manage",
		"reports:read", "reports:export",
		"audit:read",
	},
	// DevOps — the stack-operations console (pod matrix, rollout restarts,
	// cron triggers, log tails) plus the read-only surfaces needed to
	// investigate: support/config views and the audit trail. No moderation,
	// fraud, or config mutation.
	"staff:devops": {
		"ops:read", "ops:deploy",
		"incidents:read", "incidents:write",
		"support:read",
		"config:read",
		"audit:read",
	},
}

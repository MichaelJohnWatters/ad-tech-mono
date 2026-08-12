package main

import (
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
)

// NavItem is one sidebar entry. Perm (empty = always visible) gates it against
// the user's claims so the nav only shows what they can reach.
type NavItem struct {
	Label  string
	Href   string
	Icon   string
	Perm   string
	Active bool
}

// filterNav keeps only the items the claims may see (perm empty, or the user
// holds the perm, or is a "*" superuser). Nil claims → only ungated items.
func filterNav(items []NavItem, claims *auth.Claims) []NavItem {
	out := make([]NavItem, 0, len(items))
	for _, it := range items {
		if it.Perm == "" {
			out = append(out, it)
			continue
		}
		if claims != nil && (auth.HasPermission(claims, it.Perm) || auth.HasPermission(claims, "*")) {
			out = append(out, it)
		}
	}
	return out
}

// portalRevocation is the optional session-revocation checker consulted by the
// browser page gates. Set once at boot (main). Nil = no revocation (dev/tests).
// The API boundary (Auth middleware) is the authoritative enforcement; this just
// stops a revoked user from seeing the portal shell.
var portalRevocation middleware.RevocationChecker

// requireLoginPage gates a browser page on a valid session. In dev (empty
// signing key) it passes through — the Auth bypass already treats everyone as
// admin. Otherwise, a missing/invalid/revoked session redirects to /login (a
// browser redirect, not the JSON 401 the API middleware returns).
func requireLoginPage(signingKey string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if signingKey != "" {
			if _, ok := middleware.ParseSession(r, signingKey, portalRevocation); !ok {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
		}
		h(w, r)
	}
}

// advertiserNav is the full advertiser sidebar; filterNav trims it to what the
// session's claims can actually open, so a viewer never sees Billing actions
// they'd 403 on. Hrefs are hash-sections within the single portal page.
var advertiserNav = []NavItem{
	{Label: "Dashboard", Href: "#dashboard", Icon: "▤"},
	// Portfolio is agency-only (agency:read) — a roll-up across the agency's
	// managed accounts. Advertisers lack agency:read so it stays hidden.
	{Label: "Portfolio", Href: "#portfolio", Icon: "▦", Perm: "agency:read"},
	{Label: "Campaigns", Href: "#campaigns", Icon: "◎", Perm: "campaigns:read"},
	{Label: "Creatives", Href: "#creatives", Icon: "▣", Perm: "creatives:read"},
	{Label: "Audiences", Href: "#audiences", Icon: "◍", Perm: "audiences:read"},
	// Products is the DPA catalog — ingested first-party data, so it rides the
	// audience-data RBAC domain.
	{Label: "Products", Href: "#products", Icon: "▢", Perm: "audiences:read"},
	// Data marketplace (PLAN Phase 10): browse + list audience segments for sale.
	{Label: "Marketplace", Href: "#marketplace", Icon: "⇄", Perm: "marketplace:read"},
	{Label: "Conversions", Href: "#conversions", Icon: "◈", Perm: "campaigns:read"},
	{Label: "Attribution", Href: "#attribution", Icon: "◔", Perm: "reports:read"},
	{Label: "Reports", Href: "#reports", Icon: "▧", Perm: "reports:read"},
	{Label: "Billing", Href: "#billing", Icon: "▦", Perm: "billing:view"},
	{Label: "Support", Href: "#support", Icon: "✉", Perm: "support:contact"},
	{Label: "Webhooks", Href: "#webhooks", Icon: "⇄", Perm: "webhooks:read"},
	{Label: "Team", Href: "#team", Icon: "◐", Perm: "team:read"},
}

// advertiserPortalData is what advertiser.html renders with. AccountID/
// IsAdvertiser feed the page's report-query filters (advertisers see their
// tenant slice; staff/admin see platform-wide). Nav is permission-filtered.
type advertiserPortalData struct {
	AccountID    string
	IsAdvertiser bool
	Nav          []map[string]any
}

// publisherNav mirrors advertiserNav for the supply side.
var publisherNav = []NavItem{
	{Label: "Dashboard", Href: "#dashboard", Icon: "▤"},
	{Label: "Placements", Href: "#placements", Icon: "▣", Perm: "placements:read"},
	{Label: "Deals", Href: "#deals", Icon: "◈", Perm: "deals:read"},
	{Label: "Direct sold", Href: "#directsold", Icon: "◆", Perm: "deals:read"},
	{Label: "Quality", Href: "#quality", Icon: "◉", Perm: "quality:read"},
	{Label: "Audiences", Href: "#audiences", Icon: "◍", Perm: "audiences:read"},
	{Label: "Reports", Href: "#reports", Icon: "▧", Perm: "reports:read"},
	{Label: "Ad tag", Href: "#adtag", Icon: "⧉", Perm: "placements:read"},
	{Label: "Earnings", Href: "#earnings", Icon: "▦", Perm: "earnings:view"},
	{Label: "Webhooks", Href: "#webhooks", Icon: "⇄", Perm: "webhooks:read"},
	{Label: "Team", Href: "#team", Icon: "◐", Perm: "team:read"},
}

// portalData is what the portal templates render with. AccountID/IsCustomer
// feed the page's report-query filters (customer sessions see their tenant
// slice; staff/admin see platform-wide). Nav is permission-filtered.
type portalData struct {
	AccountID  string
	IsCustomer bool
	// IsAgency drives the advertiser portal's account switcher — an agency
	// session picks which managed advertiser account it acts as.
	IsAgency bool
	// Impersonating is true when a staff/admin session is viewing a persona
	// portal as another account (act-as). Drives the "Viewing as X — Stop"
	// banner; ImpersonatingID is the account being viewed.
	Impersonating   bool
	ImpersonatingID string
	// HasOpsDeploy gates the staff Ops section's action buttons (restart /
	// run-now) client-side; the API re-checks ops:deploy server-side so this
	// is presentation only. True under the dev bypass (admin view).
	HasOpsDeploy bool
	Nav          []map[string]any
}

// portalHandler renders a persona portal page with session claims driving the
// nav filter and tenant scope. customerType is the account type whose sessions
// get tenant-scoped queries. Dev bypass (no signing key) renders the admin
// view: full nav, platform-wide queries.
func portalHandler(templates *templateManager, signingKey, page string, nav []NavItem, customerType auth.AccountType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var claims *auth.Claims
		if signingKey != "" {
			claims, _ = middleware.ParseSession(r, signingKey, portalRevocation)
		}
		data := portalData{HasOpsDeploy: true}
		// Dev bypass (nil claims) = admin view: full nav. With a real
		// session, trim to the claims' permissions.
		items := nav
		if claims != nil {
			data.HasOpsDeploy = auth.HasPermission(claims, "ops:deploy") || auth.HasPermission(claims, "*")
			data.AccountID = claims.AccountID
			data.IsCustomer = claims.AccountType == customerType
			data.IsAgency = claims.AccountType == auth.AccountAgency
			// Staff/admin impersonating a customer account on a PERSONA portal
			// (not the staff console): show the full persona nav — they can open
			// every screen, and their own staff perms wouldn't pass filterNav —
			// and flag the impersonation banner. The proxy + report-scope already
			// scope the data to the target.
			if auth.IsPlatformUser(claims) && customerType != auth.AccountStaff {
				if target := middleware.ActAsTarget(r); target != "" {
					_, id := middleware.ParseActAsTarget(target)
					data.Impersonating = true
					data.ImpersonatingID = id
				}
			}
			if !data.Impersonating {
				items = filterNav(nav, claims)
			}
		}
		// app-sidebar's items are lowercase-keyed dicts (see the partial);
		// convert the filtered NavItems to that shape.
		for _, it := range items {
			data.Nav = append(data.Nav, map[string]any{
				"label": it.Label, "href": it.Href, "icon": it.Icon,
			})
		}
		templates.Render(w, page, data)
	}
}

// advertiserPortalHandler renders the advertiser portal (UI plan Phase 1).
func advertiserPortalHandler(templates *templateManager, signingKey string) http.HandlerFunc {
	return portalHandler(templates, signingKey, "advertiser.html", advertiserNav, auth.AccountAdvertiser)
}

// publisherPortalHandler renders the publisher portal (UI plan Phase 2).
func publisherPortalHandler(templates *templateManager, signingKey string) http.HandlerFunc {
	return portalHandler(templates, signingKey, "publisher.html", publisherNav, auth.AccountPublisher)
}

// partnerNav is the external-partner self-serve portal (PLAN Phase 11 #112) —
// a minimal surface: onboarding status, the integration guide, and (slice 2b)
// sandbox API keys.
var partnerNav = []NavItem{
	{Label: "Dashboard", Href: "#dashboard", Icon: "▤"},
	{Label: "Integration", Href: "#integration", Icon: "⇄", Perm: "partner:self"},
	{Label: "API keys", Href: "#apikeys", Icon: "⚿", Perm: "apikeys:manage"},
}

// partnerPortalHandler renders the external-partner portal.
func partnerPortalHandler(templates *templateManager, signingKey string) http.HandlerFunc {
	return portalHandler(templates, signingKey, "partner.html", partnerNav, auth.AccountPartner)
}

// staffNav is the operator console. Everything is permission-gated — a
// support-only role sees just what it can act on. The Tools section links
// out to the existing operator surfaces (config manager, trace explorer).
var staffNav = []NavItem{
	{Label: "Overview", Href: "#overview", Icon: "▤", Perm: "support:read"},
	{Label: "Impersonate", Href: "#impersonate", Icon: "👤", Perm: "support:read"},
	{Label: "Moderation", Href: "#moderation", Icon: "⚑", Perm: "moderation:read"},
	{Label: "Fraud rules", Href: "#fraud", Icon: "◍", Perm: "fraud:read"},
	{Label: "Revshare", Href: "#revshare", Icon: "％", Perm: "support:read"},
	{Label: "Billing terms", Href: "#billingterms", Icon: "＄", Perm: "support:read"},
	{Label: "Agencies", Href: "#agencies", Icon: "◐", Perm: "support:read"},
	{Label: "House ads", Href: "#houseads", Icon: "🏠", Perm: "support:read"},
	{Label: "Audit log", Href: "#audit", Icon: "▤", Perm: "audit:read"},
	{Label: "Profiles", Href: "#profiles", Icon: "◔", Perm: "support:read"},
	{Label: "Privacy", Href: "#privacy", Icon: "🛡", Perm: "support:read"},
	{Label: "Shading", Href: "#shading", Icon: "◑", Perm: "support:read"},
	{Label: "Retargeting", Href: "#retargeting", Icon: "🎯", Perm: "support:read"},
	{Label: "Onboarding", Href: "#onboarding", Icon: "⇥", Perm: "support:read"},
	{Label: "Demos", Href: "#demos", Icon: "🎓", Perm: "support:read"},
	{Label: "Batch runs", Href: "#batchruns", Icon: "⛓", Perm: "support:read"},
	{Label: "Support", Href: "#support", Icon: "✉", Perm: "support:read"},
	{Label: "Ops", Href: "#ops", Icon: "⎈", Perm: "ops:read"},
	{Label: "Incidents", Href: "#incidents", Icon: "⚠", Perm: "incidents:read"},
	{Label: "Partners", Href: "#partners", Icon: "⇲", Perm: "partners:read"},
	{Label: "Config", Href: "#config", Icon: "⚙", Perm: "config:read"},
	{Label: "Simulator", Href: "#simulator", Icon: "▶", Perm: "config:read"},
	{Label: "Architecture", Href: "#architecture", Icon: "🗺", Perm: "config:read"},
	{Label: "Control center", Href: "#tools", Icon: "🎛", Perm: "config:read"},
}

// staffPortalHandler renders the staff console (UI plan Phase 3). Staff is
// never tenant-scoped — AccountStaff as the "customer" type means IsCustomer
// only trips for staff accounts, and the page doesn't use PORTAL.scoped.
func staffPortalHandler(templates *templateManager, signingKey string) http.HandlerFunc {
	return portalHandler(templates, signingKey, "staff.html", staffNav, auth.AccountStaff)
}

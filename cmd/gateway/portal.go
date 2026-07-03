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

// requireLoginPage gates a browser page on a valid session. In dev (empty
// signing key) it passes through — the Auth bypass already treats everyone as
// admin. Otherwise, a missing/invalid session redirects to /login (a browser
// redirect, not the JSON 401 the API middleware returns).
func requireLoginPage(signingKey string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if signingKey != "" {
			if _, ok := middleware.ParseSession(r, signingKey); !ok {
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
	{Label: "Campaigns", Href: "#campaigns", Icon: "◎", Perm: "campaigns:read"},
	{Label: "Reports", Href: "#reports", Icon: "▧", Perm: "reports:read"},
	{Label: "Billing", Href: "#billing", Icon: "▦", Perm: "billing:view"},
}

// advertiserPortalData is what advertiser.html renders with. AccountID/
// IsAdvertiser feed the page's report-query filters (advertisers see their
// tenant slice; staff/admin see platform-wide). Nav is permission-filtered.
type advertiserPortalData struct {
	AccountID    string
	IsAdvertiser bool
	Nav          []map[string]any
}

// advertiserPortalHandler renders the advertiser portal (UI plan Phase 1) with
// session claims driving the nav and tenant scope. Dev bypass (no signing key)
// renders with an admin-shaped view: full nav, platform-wide queries.
func advertiserPortalHandler(templates *templateManager, signingKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var claims *auth.Claims
		if signingKey != "" {
			claims, _ = middleware.ParseSession(r, signingKey)
		}
		data := advertiserPortalData{}
		if claims != nil {
			data.AccountID = claims.AccountID
			data.IsAdvertiser = claims.AccountType == auth.AccountAdvertiser
		}
		// Dev bypass (nil claims) = admin view: full nav. With a real
		// session, trim to the claims' permissions.
		items := advertiserNav
		if claims != nil {
			items = filterNav(advertiserNav, claims)
		}
		// app-sidebar's items are lowercase-keyed dicts (see the partial);
		// convert the filtered NavItems to that shape.
		for _, it := range items {
			data.Nav = append(data.Nav, map[string]any{
				"label": it.Label, "href": it.Href, "icon": it.Icon,
			})
		}
		templates.Render(w, "advertiser.html", data)
	}
}

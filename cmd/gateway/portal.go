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

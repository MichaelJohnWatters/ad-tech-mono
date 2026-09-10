package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/audit"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
	"golang.org/x/crypto/bcrypt"
)

// teamMember is the subset of a users row login needs.
type teamMember struct {
	ID              string
	AccountID       string
	AccountType     auth.AccountType
	Role            auth.Role
	PasswordHash    string
	ResidencyRegion string   // accounts.residency_region — carried into the session claims
	ManagedAccounts []string // agency only — advertiser accounts it may act as
}

// userLookupFn resolves a login email to a team member (nil, nil = not found).
// Injectable so the handler is testable without a database.
type userLookupFn func(ctx context.Context, email string) (*teamMember, error)

// dbUserLookup is the real Postgres-backed lookup: active team member by email,
// joined to their account for the account type.
func dbUserLookup(db *sql.DB) userLookupFn {
	return func(ctx context.Context, email string) (*teamMember, error) {
		if db == nil {
			return nil, sql.ErrConnDone
		}
		var u teamMember
		var acctType, role string
		// Inherently pre-tenant: authentication looks a user up by email across
		// ALL accounts (there is no tenant yet), so under the NOBYPASSRLS app role
		// (security #77) it must use the platform-read hatch — otherwise RLS on
		// team_members hides every row and login is impossible. Read-only.
		err := postgres.NewFromDB(db).QueryRowPlatform(ctx, func(row *sql.Row) error {
			return row.Scan(&u.ID, &u.AccountID, &acctType, &role, &u.PasswordHash, &u.ResidencyRegion)
		},
			`SELECT tm.id::text, tm.account_id::text, a.type, tm.role, tm.password_hash, a.residency_region
			 FROM team_members tm JOIN accounts a ON a.id = tm.account_id
			 WHERE tm.email = $1 AND tm.status = 'active'`, email)
		if err == sql.ErrNoRows {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		u.AccountType = auth.AccountType(acctType)
		u.Role = auth.Role(role)
		// Agencies act on behalf of managed advertiser accounts — load the
		// assignments so the session's claims carry them (CanAccessAccount and
		// the act-as gate both read Claims.ManagedAccounts).
		if u.AccountType == auth.AccountAgency {
			managed, err := loadManagedAccounts(ctx, db, u.AccountID)
			if err != nil {
				return nil, err
			}
			u.ManagedAccounts = managed
		}
		return &u, nil
	}
}

// loadManagedAccounts returns the advertiser account IDs an agency may act as.
// Sets the tenant so the agency_managed_accounts RLS policy admits the read
// (a no-op under the dev superuser role, load-bearing under a prod app role).
func loadManagedAccounts(ctx context.Context, db *sql.DB, agencyID string) ([]string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, agencyID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT managed_account_id::text FROM agency_managed_accounts WHERE agency_account_id = $1::uuid`, agencyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// loginSubmitHandler authenticates an email+password (form or query), mints a
// JWT with the role's default permissions, sets it as an httpOnly session
// cookie, and redirects to the persona's home. Invalid credentials return 401
// without revealing whether the email or the password was wrong.
func loginSubmitHandler(lookup userLookupFn, signingKey string, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		email := r.FormValue("email")
		password := r.FormValue("password")
		if email == "" || password == "" {
			http.Error(w, "email and password required", http.StatusBadRequest)
			return
		}

		u, err := lookup(r.Context(), email)
		if err != nil {
			log.Error("login: user lookup failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if u == nil || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
			// Same response for unknown email and bad password.
			http.Error(w, "invalid email or password", http.StatusUnauthorized)
			return
		}

		now := time.Now()
		claims := &auth.Claims{
			UserID:          auth.MintUserID(u.ID),
			AccountID:       u.AccountID,
			AccountType:     u.AccountType,
			Role:            u.Role,
			Permissions:     auth.RolePermissions(u.AccountType, u.Role),
			ManagedAccounts: u.ManagedAccounts,
			ResidencyRegion: u.ResidencyRegion,
			IssuedAt:        now,
			ExpiresAt:       now.Add(12 * time.Hour),
		}
		token, err := middleware.CreateToken(claims, signingKey)
		if err != nil {
			log.Error("login: token creation failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     middleware.SessionCookieName,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Expires:  claims.ExpiresAt,
			Secure:   middleware.RequestIsSecure(r), // TLS direct OR X-Forwarded-Proto=https (behind Traefik)
		})
		log.Info("login ok", "account_type", u.AccountType, "role", u.Role)
		http.Redirect(w, r, portalHome(u.AccountType), http.StatusSeeOther)
	}
}

// logoutHandler clears the session cookie and returns to the login page. This is
// a single-device logout: the cookie is dropped from THIS browser, but the token
// itself stays valid until expiry. Use revokeSessionsHandler to kill a token
// that may still be held elsewhere (a stolen/copied session).
func logoutHandler(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: middleware.SessionCookieName, Value: "", Path: "/",
		HttpOnly: true, MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// revokeSessionsHandler kills EVERY outstanding token for a user by stamping a
// revocation checkpoint at now — any token issued before this instant (a copy
// held elsewhere included) is rejected on its next request.
//
// Default (no body / no user_id): the CALLER revokes their OWN sessions — the
// "log out everywhere / I think my session was stolen" button; their cookie is
// cleared too. With a body {"user_id":"<team-member-uuid>"} a STAFF caller
// (IsPlatformUser + support:update) revokes ANOTHER user's sessions — the
// help-desk compromise response for a user who lost their device. A non-staff
// caller supplying user_id is forbidden. Runs behind authMiddleware.
func revokeSessionsHandler(rev *middleware.RevocationStore, auditDB *sql.DB, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		// Optional cross-user target (bounded body — a tiny JSON object).
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		var req struct {
			UserID string `json:"user_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req) // empty/no body → self-revoke

		target := claims.UserID // default: self
		self := true
		if uid := strings.TrimSpace(req.UserID); uid != "" {
			// Revoking someone else's sessions is a privileged security action —
			// a platform operator (staff or admin) with the help-desk perm.
			if !auth.IsPlatformUser(claims) || !can(claims, "support:update") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			if !uuidRe.MatchString(uid) {
				http.Error(w, `{"error":"user_id must be a team-member UUID"}`, http.StatusBadRequest)
				return
			}
			target = auth.MintUserID(uid) // JWT subject = "user-"+teamMemberID
			self = target == claims.UserID
		}

		if err := rev.RevokeUser(r.Context(), target, time.Now()); err != nil {
			// A failed WRITE must be surfaced — unlike the read path, we can't
			// silently pretend the sessions were revoked.
			log.Error("revoke sessions failed", "target", target, "by", claims.UserID, "error", err)
			http.Error(w, `{"error":"could not revoke sessions"}`, http.StatusInternalServerError)
			return
		}
		if self {
			// Clear the caller's own cookie too (its token is now revoked anyway).
			http.SetCookie(w, &http.Cookie{
				Name: middleware.SessionCookieName, Value: "", Path: "/",
				HttpOnly: true, MaxAge: -1,
			})
		} else {
			// Staff revoking another user — audit the cross-user security action.
			_ = audit.Log(r.Context(), auditDB, audit.Entry{
				ActorID:      "user:" + claims.UserID,
				Action:       "auth:revoke_sessions",
				ResourceType: "user_sessions",
				ResourceID:   target,
			})
		}
		log.Info("sessions revoked", "target", target, "by", claims.UserID, "self", self)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
	}
}

// portalHome maps an account type to its landing page.
func portalHome(t auth.AccountType) string {
	switch t {
	case auth.AccountAdvertiser:
		return "/dev/portal/advertiser"
	case auth.AccountPublisher:
		return "/dev/portal/publisher"
	case auth.AccountStaff, auth.AccountAdmin:
		return "/dev/portal/staff"
	case auth.AccountPartner:
		return "/dev/portal/partner"
	case auth.AccountAgency:
		// Agencies reuse the advertiser portal; the account switcher (shown
		// only for agencies) sets the act-as account they operate as.
		return "/dev/portal/advertiser"
	default:
		return "/"
	}
}

//go:build e2e

package main

// Regression test for SSO JIT provisioning against the REAL Postgres as adtech_app.
// The holistic review found the JIT INSERT was running in a read-only tx
// (QueryRowTenantDB) → Postgres 25006, so EVERY first-time SSO login 500'd — and
// nothing caught it (the live SSO e2e can't do the IdP round-trip in-cluster; the
// pkg/ssoauth unit test never touches the DB). This exercises the exact write path.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func TestSSOJITProvisionRealDB(t *testing.T) {
	// adtech_app (NOBYPASSRLS) — the real serving role, so the tenant-GUC INSERT
	// exercises RLS too, not just the tx mode.
	db, err := sql.Open("postgres", "postgres://adtech_app:adtech-app-local@localhost:5432/adtech?sslmode=disable")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}

	uniq := time.Now().UnixNano()
	acctA := mustAccount(t, db, fmt.Sprintf("sso-jit-A-%d@e2e.local", uniq))
	acctB := mustAccount(t, db, fmt.Sprintf("sso-jit-B-%d@e2e.local", uniq))
	email := fmt.Sprintf("jit-user-%d@corp.com", uniq)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM team_members WHERE email = $1`, email)
		db.Exec(`DELETE FROM accounts WHERE id = $1::uuid OR id = $2::uuid`, acctA, acctB)
	})

	// First login → JIT creates the member (this is the INSERT that used to 25006).
	id1, role, err := ssoJITProvision(ctx, db, acctA, email, "JIT User", "analyst")
	if err != nil {
		t.Fatalf("first JIT provision failed (the read-only-tx bug?): %v", err)
	}
	if id1 == "" || role != "analyst" {
		t.Fatalf("id=%q role=%q, want non-empty id + analyst", id1, role)
	}

	// Repeat login → idempotent, same member.
	id2, _, err := ssoJITProvision(ctx, db, acctA, email, "JIT User", "analyst")
	if err != nil || id2 != id1 {
		t.Fatalf("idempotent re-provision: id=%q err=%v, want %q", id2, err, id1)
	}

	// Cross-account: the SAME email under a DIFFERENT account → rejected (no hijack).
	if _, _, err := ssoJITProvision(ctx, db, acctB, email, "JIT User", "viewer"); !errors.Is(err, errSSOCrossAccount) {
		t.Fatalf("cross-account provision: err=%v, want errSSOCrossAccount", err)
	}
}

func mustAccount(t *testing.T, db *sql.DB, email string) string {
	t.Helper()
	var id string
	// accounts has no RLS (tenant root) — a bare insert is fine.
	if err := db.QueryRow(
		`INSERT INTO accounts (name, email, type, status) VALUES ($1,$2,'advertiser','active') RETURNING id::text`,
		"SSO JIT Test", email).Scan(&id); err != nil {
		t.Fatalf("create account: %v", err)
	}
	return id
}

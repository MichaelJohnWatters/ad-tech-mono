package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdirToRepoRoot walks up from the test's CWD looking for a marker
// file (go.mod) so the relative paths inside the templateManager
// resolve regardless of where `go test` was invoked.
func chdirToRepoRoot(t *testing.T) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for dir := cwd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			t.Cleanup(func() { _ = os.Chdir(cwd) })
			if err := os.Chdir(dir); err != nil {
				t.Fatalf("chdir %s: %v", dir, err)
			}
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repo root not found from %s", cwd)
		}
		dir = parent
	}
}

func TestDict(t *testing.T) {
	cases := []struct {
		name      string
		in        []any
		wantErr   bool
		wantKey   string
		wantValue any
	}{
		{name: "empty", in: nil, wantErr: false},
		{name: "single pair", in: []any{"label", "Save"}, wantErr: false, wantKey: "label", wantValue: "Save"},
		{name: "multi pair", in: []any{"variant", "primary", "size", "md"}, wantErr: false, wantKey: "variant", wantValue: "primary"},
		{name: "odd length", in: []any{"label"}, wantErr: true},
		{name: "non-string key", in: []any{42, "value"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := dict(tc.in...)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantKey != "" {
				if got := m[tc.wantKey]; got != tc.wantValue {
					t.Errorf("m[%q] = %v, want %v", tc.wantKey, got, tc.wantValue)
				}
			}
		})
	}
}

// TestTemplateManagerLoadsAllPages confirms every .html file under
// web/templates/ parses without error. Phase 0 of the design audit
// added the manager; any future addition to web/templates that breaks
// parsing (e.g. invalid Go template syntax) gets caught here.
func TestTemplateManagerLoadsAllPages(t *testing.T) {
	chdirToRepoRoot(t)
	mgr, err := newTemplateManager(true)
	if err != nil {
		t.Fatalf("newTemplateManager: %v", err)
	}
	for _, name := range []string{"dashboard.html", "layout.html", "minimal.html", "explorer.html", "manager.html", "showcase.html", "login.html", "advertiser.html"} {
		if mgr.tmpl.Lookup(name) == nil {
			t.Errorf("template %q not loaded; available: %v", name, templateNames(mgr))
		}
	}
}

// TestShowcaseRenders executes the component-library showcase end-to-end —
// parsing (above) only proves syntax; this proves every component actually
// renders (no bad index/field access, funcs resolve) by writing it to a
// recorder and checking the output.
func TestShowcaseRenders(t *testing.T) {
	chdirToRepoRoot(t)
	mgr, err := newTemplateManager(true)
	if err != nil {
		t.Fatalf("newTemplateManager: %v", err)
	}
	rec := httptest.NewRecorder()
	mgr.Render(rec, "showcase.html", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("render status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Component Library", "Stat cards", "Summer sale", "toggleTheme()", "New campaign"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered showcase missing %q", want)
		}
	}
}

// TestAdvertiserPortalRenders executes the advertiser portal design mock — the
// component library composed into a real screen with the app-sidebar shell.
func TestAdvertiserPortalRenders(t *testing.T) {
	chdirToRepoRoot(t)
	mgr, err := newTemplateManager(true)
	if err != nil {
		t.Fatalf("newTemplateManager: %v", err)
	}
	rec := httptest.NewRecorder()
	mgr.Render(rec, "advertiser.html", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("render status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Advertiser", "Campaigns", "Spend today", "Summer sale", "New campaign"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered advertiser portal missing %q", want)
		}
	}
}

// TestPublisherPortalRenders executes the publisher portal design mock.
func TestPublisherPortalRenders(t *testing.T) {
	chdirToRepoRoot(t)
	mgr, err := newTemplateManager(true)
	if err != nil {
		t.Fatalf("newTemplateManager: %v", err)
	}
	rec := httptest.NewRecorder()
	mgr.Render(rec, "publisher.html", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("render status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Publisher", "Placements", "Earnings today", "Homepage leaderboard", "New placement"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered publisher portal missing %q", want)
		}
	}
}

// TestStaffPortalRenders executes the staff/ops console design mock.
func TestStaffPortalRenders(t *testing.T) {
	chdirToRepoRoot(t)
	mgr, err := newTemplateManager(true)
	if err != nil {
		t.Fatalf("newTemplateManager: %v", err)
	}
	rec := httptest.NewRecorder()
	mgr.Render(rec, "staff.html", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("render status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Staff", "Moderation queue", "Pending review", "audit log"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered staff portal missing %q", want)
		}
	}
}

func templateNames(m *templateManager) []string {
	var names []string
	for _, t := range m.tmpl.Templates() {
		names = append(names, t.Name())
	}
	return names
}

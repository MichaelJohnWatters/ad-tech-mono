package routes

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestOpenAPICoversManagementRoutes guards docs/openapi.yaml against route
// drift: every /v1/api/* constant declared in routes.go must appear as a path
// key in the spec. The 2026-08 sweep closed 33 silently-undocumented
// endpoints by hand — this test makes the next missing one a build failure
// instead of a future audit. Trailing slashes are normalised both ways (the
// spec documents some collections with and some without).
func TestOpenAPICoversManagementRoutes(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	spec, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}

	// Path keys in the spec: two-space-indented, start with /, end with ':'.
	specPaths := map[string]bool{}
	for _, line := range strings.Split(string(spec), "\n") {
		if strings.HasPrefix(line, "  /") && strings.HasSuffix(strings.TrimRight(line, " "), ":") {
			p := strings.TrimSuffix(strings.TrimSpace(line), ":")
			specPaths[strings.TrimSuffix(p, "/")] = true
		}
	}
	if len(specPaths) < 50 {
		t.Fatalf("parsed only %d spec paths — parser or spec broke", len(specPaths))
	}

	// Management route constants: `Name = apiPrefix + "/api/..."` in routes.go.
	re := regexp.MustCompile(`=\s*apiPrefix\s*\+\s*"(/api/[^"]+)"`)
	matches := re.FindAllStringSubmatch(string(src), -1)
	if len(matches) < 30 {
		t.Fatalf("parsed only %d /api constants from routes.go — extractor broke", len(matches))
	}
	// A constant may be a parameterised subtree root (routes.APIProfiles →
	// documented as /v1/api/profiles/{id}) — accept a spec path that extends
	// the constant with a /{param} segment.
	hasParamChild := func(base string) bool {
		for p := range specPaths {
			if strings.HasPrefix(p, base+"/{") {
				return true
			}
		}
		return false
	}
	var missing []string
	for _, m := range matches {
		full := strings.TrimSuffix(apiPrefix+m[1], "/")
		if !specPaths[full] && !hasParamChild(full) {
			missing = append(missing, full)
		}
	}
	if len(missing) > 0 {
		t.Errorf("management routes registered in pkg/routes but missing from docs/openapi.yaml "+
			"(document them — see the API drift note in the spec header):\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// readSpecPaths parses the path keys out of docs/openapi.yaml (two-space
// indent, leading /, trailing ':'), normalising trailing slashes.
func readSpecPaths(t *testing.T) map[string]bool {
	t.Helper()
	spec, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	paths := map[string]bool{}
	for _, line := range strings.Split(string(spec), "\n") {
		if strings.HasPrefix(line, "  /") && strings.HasSuffix(strings.TrimRight(line, " "), ":") {
			p := strings.TrimSuffix(strings.TrimSpace(line), ":")
			paths[strings.TrimSuffix(p, "/")] = true
		}
	}
	if len(paths) < 50 {
		t.Fatalf("parsed only %d spec paths — parser or spec broke", len(paths))
	}
	return paths
}

// TestOpenAPICoversPlatformRoutes is the forward check for the public
// platform surface OUTSIDE /v1/api/* — auth, config, the service registry,
// sellers.json and the ads.cert key. These are referenced as Go constants
// (not regex-scraped) so a renamed constant breaks the test at compile time.
func TestOpenAPICoversPlatformRoutes(t *testing.T) {
	specPaths := readSpecPaths(t)
	public := map[string]string{
		"AuthToken":          AuthToken,
		"AuthLogin":          AuthLogin,
		"AuthLogout":         AuthLogout,
		"AuthRevokeSessions": AuthRevokeSessions,
		"AuthSignup":         AuthSignup,
		"AuthBootstrap":      AuthBootstrap,
		"Config":             Config,
		"ServicesRegistry":   ServicesRegistry,
		"SellersJSON":        SellersJSON,
		"AdCertKey":          AdCertKey,
	}
	for name, path := range public {
		if !specPaths[strings.TrimSuffix(path, "/")] {
			t.Errorf("routes.%s (%s) is a public platform endpoint but has no docs/openapi.yaml entry", name, path)
		}
	}
}

// TestOpenAPIPathsAllRegistered is the REVERSE check: every path documented
// in docs/openapi.yaml must correspond to a route constant in routes.go —
// otherwise the spec rots silently when a route is removed or renamed. A
// spec path matches when it equals a constant exactly, falls under a
// declared subtree (constants with trailing slashes are registered as
// subtrees), or reduces to one after stripping trailing /{param} segments.
func TestOpenAPIPathsAllRegistered(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	specPaths := readSpecPaths(t)

	exact := map[string]bool{} // trailing-slash-stripped constant values
	subtrees := []string{}     // constants declared with a trailing slash
	add := func(v string) {
		if strings.HasSuffix(v, "/") {
			subtrees = append(subtrees, v)
		}
		exact[strings.TrimSuffix(v, "/")] = true
	}
	for _, m := range regexp.MustCompile(`=\s*"(/[^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		add(m[1])
	}
	for _, m := range regexp.MustCompile(`=\s*apiPrefix\s*\+\s*"(/[^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		add(apiPrefix + m[1])
	}
	if len(exact) < 100 {
		t.Fatalf("parsed only %d route constants from routes.go — extractor broke", len(exact))
	}

	registered := func(p string) bool {
		for {
			if exact[p] {
				return true
			}
			for _, s := range subtrees {
				if strings.HasPrefix(p+"/", s) {
					return true
				}
			}
			// Strip a trailing /{param} (or /literal after a {param}) segment
			// and retry, so /v1/api/reports/jobs/{id}/download matches the
			// /v1/api/reports/ subtree and /v1/api/deals/{id} matches
			// /v1/api/deals.
			i := strings.LastIndexByte(p, '/')
			if i <= 0 || !strings.Contains(p, "{") {
				return false
			}
			p = p[:i]
		}
	}
	var orphans []string
	for p := range specPaths {
		if !registered(p) {
			orphans = append(orphans, p)
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		t.Errorf("paths documented in docs/openapi.yaml with no registered route constant in pkg/routes "+
			"(remove the dead spec entry, or register/promote the route):\n  %s",
			strings.Join(orphans, "\n  "))
	}
}

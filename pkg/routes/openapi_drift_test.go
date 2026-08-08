package routes

import (
	"os"
	"regexp"
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

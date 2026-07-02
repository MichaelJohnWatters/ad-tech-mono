package main

import (
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// templatesRoot is the directory we walk for *.html files at load time.
// Keeping it on disk (not embedded) preserves the live-edit dev loop:
// edit minimal.html in your editor, reload the browser, see the change —
// no Go rebuild required. The Dockerfile already COPYs web/templates so
// the binary in prod still finds them at this path.
const templatesRoot = "web/templates"

// templateFuncs is the FuncMap available inside every parsed template.
// `dict` lets component partials accept named arguments via maps —
// the canonical pattern is {{ template "button" (dict "label" "Save"
// "variant" "primary") }}.
var templateFuncs = template.FuncMap{
	"dict":  dict,
	"slice": sliceOf,
}

// sliceOf collects its args into a slice — the companion to dict for passing
// a list literal to a component (e.g. a table's column headers):
//
//	{{ template "table-start" (dict "headers" (slice "Name" "Status" "Spend")) }}
func sliceOf(vals ...any) []any { return vals }

// dict converts an even-length list of key/value pairs into a map.
// Used inside templates to pass named args to {{ template "name" .args }}
// invocations, since Go's html/template stdlib doesn't ship a `dict`
// helper of its own. Errors on odd-length input rather than silently
// dropping a value.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, fmt.Errorf("dict requires an even number of args, got %d", len(kv))
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict key %d is not a string: %T", i, kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

// templateManager holds the parsed template set. In dev mode it reparses
// on every Render so editor changes show up on the next browser reload
// (no rebuild). In prod mode (devMode=false) it parses once at boot.
type templateManager struct {
	mu      sync.RWMutex
	tmpl    *template.Template
	devMode bool
}

// newTemplateManager parses every *.html under templatesRoot. Pages live
// at varying depths (web/templates/dashboard.html,
// web/templates/simulator/minimal.html, …) — each becomes a named
// template using its basename, since Go's html/template uses the file
// basename as the template name. Basenames in this tree are unique so
// no collisions today; if that changes we'll switch to relative-path
// names.
func newTemplateManager(devMode bool) (*templateManager, error) {
	m := &templateManager{devMode: devMode}
	if err := m.reload(); err != nil {
		return nil, err
	}
	return m, nil
}

// reload re-parses every template from disk. Cheap enough (<10ms total
// for the current page set) that we just do it on every Render in dev
// mode — keeps the live-edit story identical to the old http.ServeFile
// behaviour.
func (m *templateManager) reload() error {
	tmpl := template.New("").Funcs(templateFuncs)
	var paths []string
	err := filepath.Walk(templatesRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Ext(path) == ".html" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", templatesRoot, err)
	}
	if len(paths) == 0 {
		return fmt.Errorf("no .html files found under %s", templatesRoot)
	}
	if _, err := tmpl.ParseFiles(paths...); err != nil {
		return fmt.Errorf("parse templates: %w", err)
	}
	m.mu.Lock()
	m.tmpl = tmpl
	m.mu.Unlock()
	return nil
}

// Render writes the named template to w using the supplied data. In
// dev mode the template set is re-parsed first so disk edits become
// visible on the next request without a rebuild.
//
// `name` is the basename of the file (e.g. "dashboard.html",
// "minimal.html", "manager.html"). Not the relative path.
func (m *templateManager) Render(w http.ResponseWriter, name string, data any) {
	if m.devMode {
		if err := m.reload(); err != nil {
			http.Error(w, "template reload failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	m.mu.RLock()
	tmpl := m.tmpl
	m.mu.RUnlock()
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeHTML)
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		// Headers may already be written; log + best-effort error.
		// Subsequent writes to w will fail silently which is fine.
		http.Error(w, "template render failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
}

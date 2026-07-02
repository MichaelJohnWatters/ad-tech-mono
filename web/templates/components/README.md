# Component partials

Reusable Go-template partials for every UI surface. New screens **compose
these** — don't copy-paste markup between pages (that's how the pre-audit
4× duplication happened; see `docs/UI_DESIGN_AUDIT.md`).

**Living documentation: `/dev/components`** (`showcase.html`) renders every
component with its invocation next to it. When you add or change a
component, update the showcase in the same PR — it doubles as the render
test surface (`cmd/gateway/templates_test.go` parses + renders it).

## Usage

Components are parameterised with the `dict` FuncMap helper:

```go-html-template
{{ template "button" (dict "variant" "primary" "label" "Save" "onclick" "saveAll()") }}
{{ template "stat" (dict "label" "Spend today" "value" "$1,204" "delta" "+8%") }}
```

Container components come as `-start`/`-end` pairs wrapping caller markup:

```go-html-template
{{ template "card-start" (dict "title" "Campaigns") }}
  ...your content...
{{ template "card-end" . }}
```

Each partial's header comment documents its parameters — read the file,
or crib a call site from `showcase.html`.

## Inventory

| Component | Template name(s) | For |
|---|---|---|
| `badge.html` | `badge` | status chips (live/paused/pending review…) |
| `breadcrumb.html` | `breadcrumb` | page location trail |
| `button.html` | `button` | all buttons (variant/size/HTMX attrs) |
| `card.html` | `card-start` / `card-end` | panel/section container |
| `drawer.html` | `drawer` | side panel for detail/edit |
| `empty-state.html` | `empty-state` | first-run / no-rows CTA |
| `form-field.html` | `form-field` | label + input + help + error |
| `modal.html` | `modal-start` / `modal-end` | dialog frame (non-blocking) |
| `pagination.html` | `pagination` | table paging |
| `pill.html` | `pill` | filter chips (single/multi-select) |
| `search-input.html` | `search-input` | search box with fuzzy/strict toggle |
| `sparkline.html` | `sparkline` | inline-SVG time series (no chart lib) |
| `stat.html` | `stat` | KPI number + delta + sparkline slot |
| `tab-bar.html` | `tab-bar` | the Live/Secrets/Static/History pattern |
| `table.html` | `table-start` / `table-end` | data table scaffold |
| `toast.html` | `toast` container | notifications (`toast(msg, kind)` from `/static/toast.js`) |
| `toggle.html` | `toggle` | boolean switch |

## House rules (enforced by `make audit-ui`, runs in CI)

- **No inline `style="…"`** — use Tailwind utilities backed by the theme
  tokens, or a class in `theme.css`. (The handful of CSS-var inline styles
  inside modal/toast/drawer are the deliberate exception: those partials
  must render on both Tailwind pages and `theme.css` pages.)
- **No `window.confirm()` / `prompt()` / `alert()`** — toast + undo for
  reversible actions; a two-step arm/confirm click for irreversible ones
  (see `deleteSecret` in `config/manager.html`); modal partial for input.
- **No raw hex in Tailwind classes** (`bg-[#4361ee]`) — every colour comes
  from the tokens in `web/static/tokens.css` (`bg-brand`, `text-svc-dsp`,
  `bg-surface-2`…). If a colour is missing, add a token, don't inline it.
- **HTMX for partial swaps**; optimistic UI where safe.

## Adding a component

1. Create `components/<name>.html` with a `{{ define "<name>" }}` block and
   a header comment listing parameters (+ defaults).
2. Add a section to `showcase.html` demonstrating each variant.
3. If it needs behaviour, add a small `/static/<name>.js` — no frameworks.
4. `go test ./cmd/gateway/` (template parse + showcase render must pass).

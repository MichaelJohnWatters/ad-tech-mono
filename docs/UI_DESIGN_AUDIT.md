# UI Consistency Audit & Design System Recommendation

> **Status:** plan saved for later, not yet implemented. See "Migration Plan"
> for the 5-phase path. Phase 0 (wire up `template.ParseFS`) is the
> blocker for everything else.

## 1. Inventory

| Surface | URL | File | Lines | Styling | JS pattern |
|---|---|---|---:|---|---|
| Dashboard home | `/` | `cmd/gateway/main.go:308-440` (Go string literal in `dashboardHandler()`) | ~130 inline | Tailwind via CDN, brand colour inline-configured | Tiny inline `toggleTheme()` only; no HTMX, no calls to `/static/theme.js` |
| Publisher Simulator | `/dev/publisher-simulator` | `web/templates/simulator/minimal.html` | 2688 | `theme.css` CSS variables + 60 lines of bespoke inline `<style>` + massive volume of inline `style="..."` attributes (every table, every button group) | Vanilla JS, no HTMX, `window.confirm()` used in 3 places (l.1881, 2256, 2462) |
| Trace Explorer | `/dev/trace-explorer` | `web/templates/trace/explorer.html` | 286 | `theme.css` CSS variables + 24 lines of duplicated `.flow/.node/.service` CSS that already exist in `theme.css` as `.trace-flow/.trace-node/.trace-service` | Vanilla JS; renders results by string-concatenating HTML |
| Operator Console | `/dev/console`, `/dev/config-manager` | `web/templates/config/manager.html` | 1612 | Tailwind via CDN, inlined `tailwind.config` (subset of layout.html's), no `theme.css` | Vanilla JS + own `toast()` helper + 3 hand-rolled modals; no HTMX |
| Shared layout (unused) | n/a | `web/templates/layout.html` | 82 | Tailwind CDN with the canonical `tailwind.config` | `toggleTheme()` defined twice — once here, once in `/static/theme.js` |
| Static SDK | served from `/static/adtech.js` | `web/static/adtech.js` | 307 | Builds ad markup with inline `style="..."` strings | Vanilla, no framework |
| Prebid adapter | `/static/prebid-adtechmono-adapter.js` | same dir | 5928 bytes | n/a | Vanilla |

Gateway serves all four pages with raw `http.ServeFile` (`cmd/gateway/main.go:104-129`). **There is currently no Go `html/template` execution wired up** — `layout.html` exists but is dead code. This is the first thing to fix before any component partial library can exist.

## 2. Inconsistency Catalogue

| Category | Example A | Example B | Why it's a problem |
|---|---|---|---|
| **Two parallel theme systems** | `theme.css:7-87` defines `--bg-page`, `--text-primary` etc. as CSS variables, consumed by simulator + trace | `layout.html:14-33` + `config/manager.html:8-12` + `cmd/gateway/main.go:321-324` define the same brand colour via Tailwind's `tailwind.config.theme.extend.colors`. The Tailwind config is **copy-pasted three times** with slightly different subsets (manager.html omits `brand.light`, `surface.dark-2`, `surface.dark-3` and `fontFamily.mono`) | Changing `#4361ee` requires editing 4+ files; CSS-var pages and Tailwind pages can drift in shade without anyone noticing |
| **Theme toggle implemented 4 times** | `web/static/theme.js:9-18` | `layout.html:46-52` | Also in `manager.html:16` and `cmd/gateway/main.go:329`. Each version differs slightly (the inline ones flip the icon, theme.js queries `.theme-toggle`, layout.html targets `#themeIcon`) |
| **Nav bar duplicated 4 times** | `layout.html:62-73` | `cmd/gateway/main.go:334-343`, `manager.html:21-30`, `simulator/minimal.html:68-74` (uses the old `.page-header` CSS class instead) | Trace explorer uses the CSS-var nav (`explorer.html:37-42`), the other three use a Tailwind nav, and they list different links |
| **Buttons** | `theme.css:211-240` defines `.btn .btn-primary .btn-secondary .btn-ghost .btn-success` and the simulator uses those | `manager.html:36, 129, 175-176, 190, 202, 492, 1538` ships raw Tailwind utility strings: `px-3 py-1.5 text-sm bg-brand text-white rounded hover:bg-brand-hover` is repeated ~14 times | Same visual concept, two implementations; sizes drift (px-2 py-0.5 vs px-3 py-1.5 vs px-3 py-2) |
| **Modals** | `manager.html:146` uses `fixed inset-0 bg-black/50 flex items-center justify-center z-50` with click-outside-to-close inline | `simulator/minimal.html:19-21` uses `.modal-backdrop` / `.modal` CSS classes with `rgba(0,0,0,0.55)` (different alpha) | Different open/close API (`classList.toggle('hidden')` vs `classList.toggle('open')`) |
| **Toasts** | `manager.html:1486-1505` has a real toast system | Simulator uses `confirm()`/`prompt()` and the user's project memory specifically says "no confirmation dialogs — toast + undo instead" — direct violation at simulator l.1881, l.2256, l.2462, and at manager.html l.511 | Toast is not shared |
| **Tables** | `manager.html:354-388` uses `bg-gray-50 dark:bg-gray-900/50 ... uppercase tracking-wider` headers | `simulator/minimal.html:2186-2198` uses inline `style="padding: 6px 10px; background: rgba(255,255,255,0.02); text-transform: uppercase"` | No shared table component; cell padding differs (px-3 py-2 vs 6px 10px) |
| **Spacing scale** | Tailwind pages use `mb-4`, `gap-2`, `py-3` (4-pt scale) | CSS-var pages use `margin-bottom: 12px`, `padding: 16px`, `gap: 14px` | Mixing 4-pt Tailwind with arbitrary 14px values means rhythm breaks between surfaces |
| **Trace timeline duplicated** | `theme.css:313-356` defines `.trace-flow .trace-node .trace-service` | `explorer.html:14-32` re-declares `.flow .node .service`, simulator does the same at `minimal.html:35-58` | Three copies; service colours diverge — explorer uses `var(--brand-warn)` for tracker, simulator hardcodes `#f39c12` |
| **Service colour palette** | Explorer (`explorer.html:23-30`): exchange `#818cf8`, dsp `#34d399`, tracker `var(--brand-warn)` | Simulator (`minimal.html:44-49`): exchange `#818cf8`, dsp `#34d399`, tracker `#f39c12` (literal), reporting `#06b6d4` instead of `var(--brand-cyan)` (`#22d3ee`) | Same service shows two different colours depending on which page you're on |
| **Inline `style="..."`** | `simulator/minimal.html` contains ~250 occurrences of inline `style` attributes (drain banner l.198, observability block l.84-191 is ~100 lines of inline style alone) | Manager + dashboard have none | Cannot apply theme switches uniformly; light mode in simulator's observability block is fragile |
| **Font stack mismatch** | `theme.css:21` mono = `'SF Mono', Monaco, 'Cascadia Code', monospace` | `layout.html:29` mono = identical, but `manager.html:11` Tailwind config has no `fontFamily` extension so `font-mono` falls back to Tailwind default (`ui-monospace, SFMono-Regular...`) | Mono numerals don't align between surfaces |
| **Per-row controls** | Manager has them (`manager.html:457-468`) | Simulator implements them inconsistently (some rows use icon-buttons, some use `prompt()` for edit-floor at l.2238) — violates the per-row + no-prompt-dialogs preference | |

## 3. Recommended Design System

**Pick: Pure Tailwind utilities + a small Go `html/template` partial library** ("shadcn-style but for Go templates"). Reject everything else.

| Option | Bundle weight | Go template fit | HTMX fit | Dark mode | A11y | Maintenance |
|---|---|---|---|---|---|---|
| **Tailwind + Go template partials (recommended)** | 0 KB beyond Tailwind CDN | Native — `{{ template "button" . }}` | Perfect — server-rendered HTML is HTMX's whole point | `dark:` variants already work | You own it, can pin to WAI-ARIA patterns | Low: ~10 partial files, copy-paste improvements |
| Tailwind + DaisyUI | 1 extra CSS file via CDN (~70 KB minified at v5). Pure CSS, zero JS. MIT licensed. | Fine | Fine | Built-in `data-theme="dark"` / `"light"` — but uses its own theming scheme, would have to rewrite the existing class-based toggle | OK | Adds a vendor whose semantic class names (`btn-primary`, `card`, `modal`) replace utility-first thinking. Mid. |
| Tailwind + Flowbite | Tailwind CSS + Flowbite JS (~50 KB JS via CDN). MIT. | Fine | Mostly fine, but its `data-modal-target` system fights HTMX swaps (re-init needed after each swap) | Yes via `.dark` class — same scheme we already use | Good (focus trap, ESC handling) | Mid: vendor JS to monitor; re-init pattern after HTMX swaps is a footgun |
| Tailwind + Preline | ~80 KB JS via CDN. MIT. | Fine | Same HTMX re-init issue as Flowbite | Good | Good | Mid; younger library, fewer Go users |
| Tailwind + Catalyst | Requires React | n/a | n/a | n/a | n/a | **Reject — violates the no-bundler constraint** |

**Why partials win for this codebase:** existing JS interaction surface is small (open-modal, show-toast); HTMX returns HTML fragments which vendor JS libs scan-once-on-load, forcing a re-init hook after every swap; the simulator already established the "no JS framework" posture.

## 4. Theme Tokens

Brand colour `#4361ee` stays — already wired into 6+ places. Centralise to **one** place: the `tailwind.config` inline block in `layout.html`. `theme.css` shrinks to a CSS-variable bridge for any inline-styled legacy markup.

| Token group | Values |
|---|---|
| Brand | `brand.DEFAULT #4361ee`, `brand.hover #3a56d4`, `brand.light #818cf8` |
| Semantic | `success #4ade80`, `warn #fbbf24`, `error #f87171`, `info #22d3ee` |
| Service palette (for trace nodes) | `svc.exchange #818cf8`, `svc.dsp #34d399`, `svc.tracker #fbbf24`, `svc.adserver #f472b6`, `svc.reporting #22d3ee`, `svc.billing #a78bfa`, `svc.nats #fb923c` |
| Surface dark | `surface.page #0f0f1a`, `surface.dark #1a1a2e`, `surface.dark-2 #16162a`, `surface.dark-3 #1e1e3a` |
| Surface light | Drop the `dark-2`/`dark-3` suffix scheme; use Tailwind's built-in `gray-50/100/200`. |
| Border | dark: `gray.800` solid, `gray.900` subtle. light: `gray.200`/`gray.100`. |
| Spacing | Standard Tailwind 4-pt scale. **Ban arbitrary pixel values** in templates. |
| Radii | `rounded` (4px), `rounded-md` (6px), `rounded-lg` (8px). No `rounded-xl`+. |
| Typography | `text-xs`, `text-sm`, `text-base`, `text-lg`. Body 13-14, headings 16-20. Mono = SF Mono stack — explicitly add `fontFamily.mono` to every config. |

## 5. Migration Plan

### Phase 0 — Wire up Go html/template (1 day, prerequisite)
1. In `cmd/gateway/main.go`, replace the four `http.ServeFile` calls (l.104-129) with `templates := template.Must(template.ParseFS(webFS, "templates/*.html", "templates/**/*.html"))`.
2. Convert each page to use `{{ template "layout" . }}` referencing `layout.html`.
3. Move `dashboardHandler()`'s inline HTML out of `cmd/gateway/main.go` into `web/templates/dashboard.html`.
4. Add `//go:embed` for `web/templates` and `web/static` so the Dockerfile keeps working.
5. Register a `dict` template-func (4-line stdlib-free implementation).

### Phase 1 — Token & nav consolidation (1 day)
1. Centralise `tailwind.config` inline in `layout.html` only; delete copies in `manager.html:9-12` and `cmd/gateway/main.go:321-324`.
2. Add full token set (`fontFamily.mono`, full surface scale, service palette).
3. Promote `theme.css` to be **only** the CSS-var bridge.
4. Make `layout.html`'s `<nav>` the only nav; every page includes `{{ template "nav" . }}`.
5. Delete the four duplicate `toggleTheme()` implementations; keep one in `/static/theme.js`.

### Phase 2 — Component partials, applied to one surface (2 days)
**Pick the simulator first** — worst offender (2688 lines, 250 inline styles), most distinct components.

1. Create `web/templates/components/` with: `button.html`, `modal.html`, `toast.html`, `table.html`, `panel.html`, `nav.html`, `subtab-bar.html`, `card.html`, `pill.html`, `kbd-input.html`.
2. Promote the manager.html toast into `web/templates/components/toast.html` + `web/static/toast.js`.
3. Replace all `style="..."` panels in simulator with `{{ template "panel" . }}` plus utility classes.
4. Kill the `confirm()` calls at simulator:1881, 2256, 2462 and manager.html:511 — replace with toast + 5-second undo.
5. Kill the `prompt()` at simulator:2238 — replace with per-row inline edit using new modal partial.
6. Target: simulator drops from 2688 → ~1500 lines.

### Phase 3 — Apply to console + trace explorer (1.5 days)
1. `manager.html`: replace hand-rolled modals (l.146, 183, 211) with `{{ template "modal" . }}`. Replace bespoke table headers with `{{ template "table" . }}`.
2. `explorer.html`: delete duplicated trace-node CSS at l.14-32; rely on `theme.css`'s `.trace-flow`/`.trace-node`. Use `nav` + `panel` partials.
3. (Stretch) Replace string-concatenation HTML in trace explorer's `renderSteps` (l.113) with `{{ template "trace-row" . }}` per hop; JS pushes data via HTMX swap.

### Phase 4 — Lint guardrails (0.5 day)
1. CI grep: `grep -rn 'style="' web/templates/` must be < N (ratchets down each PR).
2. CI grep: ban `confirm(` and `prompt(` in `web/templates/`.
3. CI grep: ban `bg-#[0-9a-f]` (raw hex on Tailwind classes).
4. Add a `make audit-ui` target.

**Total: ~5 person-days.** Phase 0 is the only blocker; everything after is independently shippable.

## 6. Stretch — Component Partial Structure

```
web/templates/
  layout.html                  -- <html>, <head>, nav, footer, slot for {{ .Content }}
  dashboard.html               -- lifted out of gateway main.go
  components/
    button.html                -- {{ template "button" (dict "variant" "primary" "label" "Save") }}
    modal.html                 -- backdrop + close handler + named slot
    toast.html                 -- container + JS already in /static/toast.js
    table.html                 -- thead/tbody scaffold
    panel.html                 -- metric panel with label/value/colour
    nav.html                   -- top nav, theme toggle button
    subtab-bar.html            -- the Live/Secrets/Static/History pattern
    pill.html                  -- service picker pill / status pill
    kbd-input.html             -- search input with mode toggle (fuzzy/strict)
    trace-row.html             -- one node in a trace flow
  simulator/minimal.html       -- thin orchestration over partials
  trace/explorer.html          -- thin orchestration over partials
  config/manager.html          -- thin orchestration over partials
```

### Example: `web/templates/components/button.html`

```html
{{/* button: variant=primary|secondary|ghost|danger, size=sm|md|lg, label, onclick, type */}}
{{ define "button" }}
{{ $base := "inline-flex items-center justify-center gap-1.5 font-medium rounded-md transition-colors focus:outline-none focus:ring-2 focus:ring-brand/40 disabled:opacity-50 disabled:cursor-not-allowed" }}
{{ $variant := index (dict
    "primary"   "bg-brand text-white hover:bg-brand-hover"
    "secondary" "bg-white dark:bg-surface-dark text-gray-700 dark:text-gray-200 border border-gray-200 dark:border-gray-700 hover:bg-gray-50 dark:hover:bg-gray-800"
    "ghost"     "bg-transparent text-gray-500 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-800"
    "danger"    "bg-red-500 text-white hover:bg-red-600"
) (or .variant "primary") }}
{{ $size := index (dict "sm" "px-2.5 py-1 text-xs" "md" "px-3 py-1.5 text-sm" "lg" "px-4 py-2 text-sm") (or .size "md") }}
<button type="{{ or .type "button" }}"
        {{ with .id }}id="{{ . }}"{{ end }}
        {{ with .onclick }}onclick="{{ . }}"{{ end }}
        {{ with .hx_get }}hx-get="{{ . }}"{{ end }}
        {{ with .hx_post }}hx-post="{{ . }}"{{ end }}
        {{ with .hx_target }}hx-target="{{ . }}"{{ end }}
        class="{{ $base }} {{ $variant }} {{ $size }}">
  {{- .label -}}
</button>
{{ end }}
```

Usage:

```go-html-template
{{ template "button" (dict "variant" "primary" "label" "Save" "onclick" "saveAll()") }}
{{ template "button" (dict "variant" "secondary" "size" "sm" "label" "Refresh" "hx_post" "/v1/refresh" "hx_target" "#content") }}
```

The same shape works for modal/toast/panel. Need a `dict` FuncMap entry — well-known 4-line implementation.

## Critical Files for Implementation

- `cmd/gateway/main.go` — replace `http.ServeFile` with `template.ParseFS`, remove inline `dashboardHandler()` HTML, register `dict` FuncMap
- `web/templates/layout.html` — becomes the single source of nav, theme toggle, Tailwind config
- `web/static/theme.css` — slim down to brand-token CSS-var bridge only
- `web/templates/simulator/minimal.html` — biggest cleanup target; first surface to migrate
- `web/templates/config/manager.html` — modal/toast/table extraction target

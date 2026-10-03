# C4 model expansion — service-by-service prompt

Paste the block below into a fresh context to flesh out the C4 model. It's pure docs work
(read code + edit one `.dsl` file) — no stack, no deploy, no build required.

---

```
# Task: expand the Structurizr C4 model of ad-tech-mono, one service at a time

## Context
We're modelling this Go monorepo (/Users/michaeljohnwatters/repo/ad-tech-mono) as a single C4
model in Structurizr DSL. A starter exists at `docs/diagrams/workspace.dsl` with:
  - the System **Context** view (actors + external systems),
  - the **Container** view (all services + datastores + the serving/event/money edges), and
  - ONE worked **Component** view: the DSP (its hot-path bid loop).

Your job: add a **Component view for each remaining service**, by reading that service's code and
its per-directory `CLAUDE.md`, and appending `component` blocks + a `component` view to
`docs/diagrams/workspace.dsl`. Keep the single model consistent — this is the whole point of C4
(one source, many zoom levels, no drift).

## Method — ONE service per pass (don't try to do them all at once)
For each service below, in order:
1. Read `cmd/<svc>/CLAUDE.md` (the "Responsibilities" / "Key packages" sections are your component
   hints) AND the actual `cmd/<svc>/*.go` + the main `pkg/*` it uses (+ those pkg `CLAUDE.md`s).
2. Identify the **4–8 MAJOR internal components** — real code units: HTTP/gRPC handlers, an engine
   (`pkg/targeting`, `pkg/auction`, `pkg/bidshading`…), a warm cache / refresher, a NATS consumer,
   a store adapter. Don't over-decompose; don't invent anything not in the code.
3. Inside that container's block in `workspace.dsl`, add the `component` declarations.
4. Add the component→component relationships, plus any container-level edges that become clearer
   (reuse the existing container identifiers exactly, e.g. `postgres`, `redis`, `nats`, `clickhouse`).
5. Add a `component <container> "<Key>" "<desc>" { include *; autolayout lr }` view in the `views` block
   (there's a `# TODO` marker listing all the views to add).
6. Verify every relationship against the code (don't guess edges).
7. Commit that ONE service: `docs(c4): component view for <svc>` on `main`, trailer
   `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. Small, reviewable commits.
   (If a push races another context, `git pull --rebase` — it's a single doc file, no real conflict.)

## Service order
ssp → exchange → adserver → pubAdserver → reporting → tracker → gateway → pipeline →
identityConsumer → audienceRT → reportRunner → ssai → transcoder → webhooks → notifications.
(DSP is already done as the example — match its style.)

## Conventions (keep the model valid + consistent)
- **Identifiers are GLOBAL in Structurizr** — prefix every NEW component id with its service to avoid
  collisions, e.g. `ssp_segmentResolver`, `exch_router`, `rep_billingEngine`. (The starter's DSP
  components are unprefixed; leave them, just prefix the new ones.)
- Descriptions: one short line, end with the source in parens — e.g.
  `component "SmartRouter" "Skips slow/no-bid DSP legs; warm-starts from dsp_calls. (pkg/auction/router.go)" "Go"`.
- Reuse container ids from the starter; tag datastores `"Datastore"`, externals `"External"`.
- Don't modify the existing D2 `.d2` files — this is a parallel C4 prototype, not a migration.

## Structurizr DSL cheat-sheet (so you don't need to look it up)
- Structure: `workspace "n" "d" { model { … } views { … } }`.
- Elements (assign an identifier so you can reference it in relationships):
  - `x = person "Name" "Desc"`
  - `x = softwareSystem "Name" "Desc" "Tags" { …containers… }`
  - `x = container "Name" "Desc" "Technology" "Tags" { …components… }`
  - `x = component "Name" "Desc" "Technology" "Tags"`
- Relationship: `src -> dest "Desc" "Technology"` (identifiers must already be declared).
- Views: `systemContext|container|component <scope> "Key" "Desc" { include *; autolayout lr }`.
  A `component` view's scope is a CONTAINER id; `include *` pulls that container's components + the
  elements they connect to. View "Key"s must be unique.
- Tags drive `styles { element "Tag" { … } }` (already defined in the starter).

## Validation
- Keep the DSL syntactically valid: balanced braces, every relationship identifier declared, unique
  view keys + element ids.
- If the Structurizr CLI or `structurizr-lite` (Docker) is installed you may validate/preview — BUT
  that's host-CPU; if a perf benchmark/load test is running on this host, SKIP the live preview and
  just keep the DSL careful. (`docker run -it --rm -p 8080:8080 -v $(pwd)/docs/diagrams:/usr/local/structurizr structurizr/lite`
  then open localhost:8080 — do this only when the host is idle.)
- Do NOT run any stack command (make deploy / reset / loadtest) — this task is docs-only.

## Inventory / map (where to look)
- `docs/diagrams/README.md` — the diagram index + per-diagram scope.
- The existing `docs/diagrams/*.d2` + `*.md` — they already encode the services + edges; cross-check.
- `cmd/CLAUDE.md` — the master table of every service/job and what it does.
- Per-service `cmd/<svc>/CLAUDE.md` and per-package `pkg/<pkg>/CLAUDE.md`.

## Acceptance
- `docs/diagrams/workspace.dsl` has a component view for every service listed above.
- Every component maps to a real code unit; every relationship is verified against code.
- DSL stays valid (parses); one small commit per service on `main`.
```

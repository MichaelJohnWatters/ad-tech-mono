.PHONY: setup proto test test-integration lint build seed simulate reset demo-reset diagrams chaos help ssai-smoke stack-images stack-up stack-down stack-doctor deploy deploy-demosites deploy-demostore hosts security-demo devconsole demo-forward demo-setup demosite demosites extbidder demoadv security-harness secrets-encrypt secrets-edit secrets-decrypt deploy-staging

# --- Setup ---
setup: ## Install prerequisites and start local k3s
	@echo "Installing prerequisites..."
	brew install go colima kubectl tilt buf golangci-lint d2 k6 || true
	colima start --kubernetes --cpu 4 --memory 16 --disk 100
	@echo "Setup complete. Run 'tilt up' to start the stack."

# --- Code Generation ---
proto: ## Regenerate Go code from proto files (via Buf)
	buf generate
	@echo "Proto generation complete."

diagrams: ## Regenerate SVG diagrams from D2 source, then sync into the staff portal
	@for f in docs/diagrams/*.d2; do \
		name=$$(basename "$$f" .d2); \
		echo "Rendering $$name..."; \
		d2 --layout elk --pad 40 "$$f" "docs/diagrams/$${name}.svg"; \
	done
	@echo "Syncing to web/static/diagrams (served by the staff-portal Architecture tab)..."
	@mkdir -p web/static/diagrams
	@# Mirror exactly (delete stale artifacts for diagrams removed from docs/).
	@rm -f web/static/diagrams/*.svg
	@cp docs/diagrams/*.svg web/static/diagrams/
	@# .md diagrams the portal renders (manifest type: mermaid) — data-lifecycle
	@# is the ★ headline (portal extracts its ```mermaid block).
	@cp docs/diagrams/data-lifecycle.md docs/diagrams/e2e-trace.md docs/diagrams/data-reporting.md docs/diagrams/end-to-end-flow.md web/static/diagrams/ 2>/dev/null || true
	@# d2 writes SVGs mode 600 (owner-only); the gateway runs non-root in-container
	@# and 403s on unreadable files. Force world-readable so the portal can serve them.
	@chmod 644 docs/diagrams/*.svg web/static/diagrams/*.svg 2>/dev/null || true
	@echo "Diagrams regenerated + synced. (manifest: web/static/diagrams/manifest.json)"

c4: ## Export the C4 model (docs/diagrams/workspace.dsl) to mermaid + sync into the staff portal
	@echo "Exporting C4 model via structurizr/structurizr (the maintained image — cli/lite are dead)..."
	@docker run --rm -v "$(PWD)/docs/diagrams:/data" structurizr/structurizr export -workspace /data/workspace.dsl -format mermaid -output /data >/dev/null 2>&1 || { echo "structurizr export failed (Docker up? DSL valid?)"; exit 1; }
	@echo "Wrapping mermaid exports as portal .md + syncing..."
	@mkdir -p web/static/diagrams
	@rm -f web/static/diagrams/structurizr-*.md
	@for f in docs/diagrams/structurizr-*.mmd; do \
		name=$$(basename "$$f" .mmd); \
		{ printf '```mermaid\n'; cat "$$f"; printf '\n```\n'; } > "web/static/diagrams/$${name}.md"; \
	done
	@rm -f docs/diagrams/structurizr-*.mmd
	@echo "C4 synced to web/static/diagrams/structurizr-*.md. Keep manifest.json C4 entries in sync (type: mermaid)."

# --- Testing ---
test: ## Run unit tests
	go test ./pkg/... ./cmd/...

test-integration: ## Run integration tests (requires Docker for testcontainers)
	go test ./pkg/... ./cmd/... -tags=integration -count=1

test-duckdb: ## Run the CGO DuckDB analytics tests (requires a C toolchain)
	CGO_ENABLED=1 go test -tags=duckdb ./pkg/store/analytics/... ./cmd/reporting/... -count=1

test-clickhouse: ## Run the ClickHouse analytics integration tests (needs a live CH; tilt forwards :9010)
	CLICKHOUSE_ADDR=$${CLICKHOUSE_ADDR:-127.0.0.1:9010} go test -tags=clickhouse_integration ./pkg/store/analytics/... -count=1 -run ClickHouse

test-e2e: ## Run end-to-end tests against the live stack (real ClickHouse backend; preflight only fail-fasts; chaos tests excluded — see test-e2e-chaos)
	./scripts/e2e-preflight.sh
	# 20m, not 10m: the full ~190-test suite runs ~16m. A too-tight timeout makes
	# go test KILL the binary mid-test, which SKIPS t.Cleanup and leaves the stack
	# dirty (stale config like tracker.signature_validation, polluted router stats)
	# → a cascade of false failures on the next run. Give it headroom.
	go test ./tests/... -tags=e2e -count=1 -timeout=20m

test-e2e-chaos: ## Chaos e2e: kills infra pods (nats/postgres/redis/minio) to prove fail-open behaviour. Run SEPARATELY — the pod churn flaps port-forwards and destabilises unrelated tests, so these are opt-in (E2E_CHAOS=1) and excluded from test-e2e.
	E2E_CHAOS=1 go test ./tests/e2e -tags=e2e -count=1 -timeout=15m -run 'TestChaos'

test-e2e-security: ## Security/auth e2e subset against the live stack: authn (API keys, session cookie), session revocation (self + staff cross-user), SSP consent gate, forged-identity-header strip, per-pod + distributed rate limits, tenant isolation, RBAC, SQL-injection safety, CSRF. A curated -run list — add new security tests here.
	./scripts/e2e-preflight.sh
	go test ./tests/e2e -tags=e2e -count=1 -timeout=10m -run 'TestSessionRevocation|TestStaffRevokesAnotherUsersSessions|TestSessionCookieTamperRejected|TestSSPConsentGatesSegmentsToExternalDSP|TestGatewayStripsForgedIdentityHeaders|TestRateLimitPerIP|TestDistributedRateLimitSharedAcrossPods|TestAuthCRUD|TestCSRFCrossSiteBlocked|TestViewerRoleCannotMutateCampaign|TestTenantIsolationAudienceList|TestCampaignNameInjectionSafe|TestTrackerHMACStrictMode|TestConversionPerAdvertiserKeyRejectsForgery|TestBeaconRejectsTamperedPrice|TestBeaconRejectsSpoofedPublisher|TestBeaconSignedImpressionRecordsOnce'

test-e2e-hotcold: ## Long-running hot/cold store e2e (~3min). Needs a cold-store-enabled clickhouse stack. Sets a short hot_window for speed, then restores it. The test reads the ACTUAL deployed window, so it stays correct if this value is overridden. Cold reads are ClickHouse s3() over the Parquet export (ADR 0006); the test drives an export so the aged burst is in the archive.
	kubectl set env deployment/reporting -n adtech REPORTING_HOT_WINDOW=90s
	kubectl rollout status deployment/reporting -n adtech --timeout=150s
	-go test ./tests/e2e/ -tags=e2e -run TestHotColdStore -count=1 -timeout=10m -v
	kubectl set env deployment/reporting -n adtech REPORTING_HOT_WINDOW=336h
	kubectl rollout status deployment/reporting -n adtech --timeout=150s

ssai-smoke: ## R1 live smoke: real stack conditions + serves a decodable ad segment (needs tilt up + ffmpeg)
	./scripts/ssai-smoke.sh

viewability-smoke: ## Real headless-Chrome smoke: a browser plays a video via the adtech.js SDK, its IntersectionObserver fires the viewability beacon, and it lands in ClickHouse (needs stack up + seeded + Chrome). Rebuild the gateway first if you changed web/static/adtech.js.
	./scripts/viewability-smoke.sh

advertiser-smoke: ## Real headless-Chrome smoke: a browser on the demo advertiser site fires the retargeting + conversion pixels via the shared adtech-adv.js tag, and both land in ClickHouse (needs stack up + seeded + Chrome).
	./scripts/advertiser-smoke.sh

asciline-demo: ## Ad-free ASCILINE reference testbed on :8000 (clones to third_party/; content only — never the ad path)
	./scripts/asciline-demo.sh

test-all: test test-integration test-duckdb test-e2e ## Run all test layers

# --- Linting ---
lint: ## Run golangci-lint + buf lint
	golangci-lint run ./...
	buf lint

audit-ui: ## Lint the web/ templates: inline styles, confirm/prompt, arbitrary hex
	@bash scripts/audit-ui.sh

# --- Building ---
build: ## Build all service binaries
	@for svc in dsp ssp exchange adserver tracker reporting gateway pipeline webhooks ssai transcoder; do \
		echo "Building $$svc..."; \
		go build -o bin/$$svc ./cmd/$$svc; \
	done
	@echo "All services built."

build-reporting-duckdb: ## Build reporting with the durable DuckDB analytics backend (CGO)
	CGO_ENABLED=1 go build -tags duckdb -o bin/reporting ./cmd/reporting
	@echo "Built bin/reporting with duckdb backend. Set reporting.analytics_backend=duckdb to use it."

build-images: ## Build all Docker images locally
	@for svc in dsp ssp exchange adserver tracker pipeline webhooks ssai; do \
		docker build --build-arg SERVICE=$$svc -f build/Dockerfile -t adtech-$$svc .; \
	done
	docker build -f build/Dockerfile.gateway -t adtech-gateway .
	docker build -f build/Dockerfile.reporting -t adtech-reporting .
	docker build -f build/Dockerfile.transcoder -t adtech-transcoder .
	@echo "All images built."

# --- Database ---
migrate: ## Run database migrations
	go run ./cmd/migrate

migrate-status: ## Show migration status
	go run ./cmd/migrate status

# --- Seed & Simulation ---
demo: ## One-command rich local setup: seed + baseline traffic (needs `tilt up`). Everyone runs this.
	bash scripts/demo.sh

reset: ## Wipe ALL data (Postgres+ClickHouse+Redis) then re-populate — a clean fresh run.
	bash scripts/reset.sh

demo-reset: ## Reliable demo reset: in-cluster wipe+reseed + warm SSAI content/ad videos + verify the first-party-audience (Lumière) demo. Works after a plain run OR a PURGE teardown; no demo-forward needed.
	bash scripts/demo-reset.sh

demo-warm: ## Non-destructive: seed (UPSERT) + warm caches + SSAI content + prewarm an ALREADY-running stack to full browsability. No wipe, no demo-forward. STOPS before traffic (run a load test after). Use after an e2e left the DB bare.
	bash scripts/demo-warm.sh

traffic: ## Continuous simulator at a set speed (override with DEMO_RPS, default 5). Ctrl-C to stop.
	go run ./cmd/simulator run --profile steady --duration 12h --rps $${DEMO_RPS:-5}

seed: ## Seed database with standard profile
	go run ./cmd/seed --profile standard

seed-minimal: ## Seed database with minimal profile
	go run ./cmd/seed --profile minimal

seed-stress: ## Seed database with stress profile
	go run ./cmd/seed --profile stress

simulate: ## Start steady simulation
	go run ./cmd/simulator --profile steady

simulate-trickle: ## Start trickle simulation (1 req/sec)
	go run ./cmd/simulator --profile trickle

simulate-burst: ## Start burst simulation
	go run ./cmd/simulator --profile burst

# --- Performance Testing ---
perf-tracker: ## k6 load on tracker pixel ingest (tweak: RPS=200 DURATION=2m)
	k6 run tests/k6/tracker-load.js

perf-exchange: ## k6 load on exchange auctions with REAL seeded placements + schain (tweak: RPS=100 DURATION=5m)
	PLACEMENTS="$$(scripts/k6-placements.sh)" k6 run tests/k6/exchange-load.js

perf-all: perf-tracker perf-exchange ## Run all k6 load tests

loadtest: ## Full-path load via the simulator (auction→serve→beacons→reporting): make loadtest RPS=100 DURATION=10m; add VERIFY=1 to assert reporting counts match; USER_POOL=N draws user ids from the seeded synthetic universe (audience density runs)
	go run ./cmd/simulator run --profile steady --rps $${RPS:-100} --duration $${DURATION:-10m} $$( [ "$${VERIFY}" = "1" ] && echo --verify ) $$( [ -n "$${USER_POOL}" ] && echo --user-pool $${USER_POOL} )

loadtest-ramp: ## Progressive full-path load: stages through RPS_STAGES (default "100 150 250") × STAGE_DURATION (default 5m), verifying pipeline counts between stages; aborts on degradation
	scripts/loadtest-ramp.sh

browser-test: ## Portal data-visibility in a real browser (advertiser + publisher logins, staff impersonation of both); run after seed/traffic. Needs node>=20 (uses brew node@23 if nvm default is older)
	cd tests/browser && PATH=/usr/local/opt/node@23/bin:$$PATH npm install --silent && PATH=/usr/local/opt/node@23/bin:$$PATH npx playwright test

# --- Chaos Testing ---
chaos: ## Run chaos test (usage: make chaos profile=redis_failure)
	go run ./cmd/simulator --profile steady --duration 5m --chaos $(profile)

chaos-verify: ## Verify chaos test pass criteria
	go run ./cmd/simulator --mode=chaos-verify

# --- A/B Testing ---
ab-test: ## Run A/B test (usage: make ab-test service=dsp)
	@echo "Deploying canary for $(service)..."
	kubectl apply -f k8s/base/$(service)/deployment-canary.yaml
	go run ./cmd/simulator --profile steady --duration 5m
	go run ./cmd/reporting --mode=ab-compare --service=$(service)

ab-test-chaos: ## Run A/B test with chaos (usage: make ab-test-chaos service=dsp chaos=redis_failure)
	kubectl apply -f k8s/base/$(service)/deployment-canary.yaml
	go run ./cmd/simulator --profile steady --duration 5m --chaos $(chaos)
	go run ./cmd/reporting --mode=ab-compare --service=$(service)

# --- Helm dev loop (Rancher Desktop / k3s) ---
# The Tilt replacement: build images with scripts/stack-images.sh, deploy
# with helm. kubectl/docker context must be rancher-desktop (stack-up checks).

devconsole: ## Host dev-loop UI (build/deploy buttons) at http://localhost:8099
	go run ./cmd/devconsole

stack-images: ## Build all adtech-* service images (host cross-compile + tiny images)
	scripts/stack-images.sh

stack-up: ## Deploy/upgrade the full local stack via helm (builds images first)
	@[ "$$(kubectl config current-context)" = "rancher-desktop" ] || { echo "kubectl context is not rancher-desktop (run: kubectl config use-context rancher-desktop)"; exit 1; }
	scripts/stack-images.sh
	helm upgrade --install adtech k8s/helm/adtech --timeout 10m
	@echo "waiting for migrations + core services…"
	@# The migrate hook job deletes itself on success (and is created after
	@# helm returns), so wait on the REAL condition: the goose schema exists.
	@for i in $$(seq 1 90); do \
	  kubectl -n adtech exec postgres-0 -- psql -U adtech -d adtech -tAc "SELECT 1 FROM goose_db_version LIMIT 1" >/dev/null 2>&1 && break; \
	  sleep 5; \
	done
	kubectl -n adtech rollout status deploy/gateway deploy/reporting --timeout=300s
	@if [ -f dev/tls/localhost.pem ]; then \
	  kubectl create secret generic gateway-tls -n adtech \
	    --from-file=cert.pem=dev/tls/localhost.pem \
	    --from-file=key.pem=dev/tls/localhost-key.pem \
	    --dry-run=client -o yaml | kubectl apply -f - >/dev/null && echo "gateway-tls secret applied"; \
	  kubectl create secret tls adtech-tls -n adtech \
	    --cert=dev/tls/localhost.pem --key=dev/tls/localhost-key.pem \
	    --dry-run=client -o yaml | kubectl apply -f - >/dev/null && echo "adtech-tls (ingress) secret applied"; \
	else echo "dev/tls/localhost.pem missing — run scripts/gen-dev-tls.sh (mkcert) for HTTPS ingress"; fi
	@if [ "$(DEMOSITES)" = "1" ]; then $(MAKE) deploy-demosites; fi

deploy-demosites: ## Deploy/refresh the 6 in-cluster demo publisher sites (own `demosites` ns, HTTPS from dev/tls). Scoped apply — never touches adtech-core. Needs the stack up + seeded (profiles/publishers/demosites.yaml).
	@[ "$$(kubectl config current-context)" = "rancher-desktop" ] || { echo "kubectl context is not rancher-desktop"; exit 1; }
	@[ -f dev/tls/localhost.pem ] || { echo "dev/tls/localhost.pem missing — run scripts/gen-dev-tls.sh (mkcert) for HTTPS demo sites"; exit 1; }
	scripts/stack-images.sh demosite
	@# The demosites Ingress needs the wildcard cert COPIED into its namespace as a
	@# base64 `data:` Secret; feed it from the same dev/tls material as adtech-tls.
	@# Scoped `helm template … --show-only | kubectl apply` deploys ONLY the
	@# demosites objects — it deliberately avoids a full `helm upgrade`, which can
	@# conflict on fields a prior `kubectl set env` (pre-fix demo-setup) owned.
	@CRT=$$(base64 < dev/tls/localhost.pem | tr -d '\n'); \
	 KEY=$$(base64 < dev/tls/localhost-key.pem | tr -d '\n'); \
	 helm template adtech k8s/helm/adtech --set global.demosites=true --set tls.enabled=true \
	   --set-string demosites.tls.crt="$$CRT" --set-string demosites.tls.key="$$KEY" \
	   --show-only templates/demosites.yaml | kubectl apply -f -
	@for d in $$(kubectl -n demosites get deploy -o name 2>/dev/null); do kubectl -n demosites rollout status $$d --timeout=180s; done
	@echo "demo sites → https://{chronicle,gadget,primereel,soundwave,twitchr,viewtube}.adtech.local"
	@echo "browse the demo sites by name after: make hosts"
	@$(MAKE) deploy-demostore

deploy-demostore: ## Deploy/refresh the in-cluster demo ADVERTISER shop (cmd/demoadv, shop.adtech.local) — the EARNED-audience demo. Same `demosites` ns + scoped apply; points at the real adv-barkbox advertiser. Needs the stack up + seeded.
	@[ "$$(kubectl config current-context)" = "rancher-desktop" ] || { echo "kubectl context is not rancher-desktop"; exit 1; }
	@[ -f dev/tls/localhost.pem ] || { echo "dev/tls/localhost.pem missing — run scripts/gen-dev-tls.sh (mkcert) for HTTPS demo shop"; exit 1; }
	scripts/stack-images.sh demoadv
	@# Scoped `helm template … --show-only | kubectl apply` deploys ONLY the
	@# demostore objects (same pattern + copied wildcard cert as demo sites).
	@CRT=$$(base64 < dev/tls/localhost.pem | tr -d '\n'); \
	 KEY=$$(base64 < dev/tls/localhost-key.pem | tr -d '\n'); \
	 helm template adtech k8s/helm/adtech --set global.demosites=true --set tls.enabled=true \
	   --set-string demostore.tls.crt="$$CRT" --set-string demostore.tls.key="$$KEY" \
	   --show-only templates/demostore.yaml | kubectl apply -f -
	@kubectl -n demosites rollout status deploy/demostore-shop --timeout=180s
	@echo "demo shop → https://shop.adtech.local  (EARNED-audience demo: browse → real-time enroll → chased → suppressed)"
	@echo "browse it by name after: make hosts"

hosts: ## Point every *.adtech.local ingress host (core + demo sites, derived from the chart) at the local Traefik ingress in /etc/hosts (idempotent; needs sudo). Override IP with ADTECH_HOSTS_IP.
	bash scripts/hosts-setup.sh

security-demo: ## LIVE "try to break it" run: fires real forgery/tamper/spoof/fraud/input attacks at the money+attribution paths and shows each rejected (fill-independent — constructs its own signed beacon). Needs `make demo-forward`. Full 9-attack proof = make test-e2e-security.
	go run ./cmd/securitydemo

demo-forward: ## SETUP/TOOLING ONLY (NOT needed to browse the demo — sites/portal work via *.adtech.local). Bridges host→cluster on localhost for CLI tools (seed/simulator/prewarm/e2e). Run in its OWN terminal; Ctrl-C stops.
	bash scripts/demo-forward.sh

demo-setup: ## Interview-ready: seed + warm (prewarm/SSAI) + baseline traffic + VERIFY every ad format serves. Needs `make demo-forward` running.
	bash scripts/demo-setup.sh

test-portal: ## Browser smoke of the advertiser portal (chromedp): dead-onclick regression, audience upload-append, campaign page + pickers. Needs stack up + seeded.
	go run ./cmd/portalsmoke

demo-loadtest: ## Interview grand-finale: short (~4m) soak tuned for SPREAD (competitive bids + asap pacing + freq caps) so dashboards come alive. Tune RPS/DURATION/USER_POOL. Needs `make demo-forward`.
	bash scripts/demo-loadtest.sh

demo-cmaf: ## Opt-in: make the SSAI "Video · DASH (CMAF)" path real — package an fMP4/CMAF content origin + warm the fMP4 ad conditioning. Run after demo-setup. Needs `make demo-forward`.
	bash scripts/ssai-cmaf.sh

demo-data: ## Light up wired-but-empty portal sections (invoices/conversions/webhooks) + purge stale marketplace rows. Run after demo-setup + a load test. Needs `make demo-forward`.
	bash scripts/demo-data-gen.sh

demosite: ## Run the external demo publisher site (host process, :9000). Needs the stack up + seeded.
	@echo "demosite (external publisher) → http://localhost:9000  (Ctrl-C to stop)"
	@echo "for the public TLS path: see cmd/demosite/README.md"
	go run ./cmd/demosite

demosites: ## Run the 3 branded external publisher sites as separate origins (chronicle:9001, gadget:9002, streamhub:9003). Needs the stack up + seeded.
	@echo "3 external publisher sites → chronicle http://localhost:9001 · gadget http://localhost:9002 · streamhub http://localhost:9003"
	@echo "(Ctrl-C stops all three)"
	@trap 'kill 0' INT TERM; \
	 DEMOSITE_SITE=chronicle DEMOSITE_PORT=9001 go run ./cmd/demosite & \
	 DEMOSITE_SITE=gadget    DEMOSITE_PORT=9002 go run ./cmd/demosite & \
	 DEMOSITE_SITE=streamhub DEMOSITE_PORT=9003 go run ./cmd/demosite & \
	 wait

extbidder: ## Run the external DSP partner bidder (host process, :9100). Wire it in: cmd/extbidder/README.md
	@echo "extbidder (external DSP) → :9100  (Ctrl-C to stop)"
	@echo "add http://host.docker.internal:9100 to exchange.dsp_endpoints — see cmd/extbidder/README.md"
	go run ./cmd/extbidder

demoadv: ## Run the external demo advertiser site "Ford" (host process, :9200). Fires retargeting + conversion pixels.
	@echo "demoadv (external advertiser) → http://localhost:9200  (Ctrl-C to stop)"
	@echo "visit pages → retargeting audience builds; see cmd/demoadv/README.md"
	go run ./cmd/demoadv

security-harness: ## Toggle the anti-spoofing enforcement test harness: make security-harness ARG=on|off|status
	scripts/security-harness.sh $(or $(ARG),status)

deploy: ## Rebuild ONE service image + restart it: make deploy SVC=pipeline
	@[ -n "$(SVC)" ] || { echo "usage: make deploy SVC=<service>"; exit 1; }
	scripts/stack-images.sh $(SVC)
	@if [ "$(SVC)" = dsp ]; then kubectl -n adtech rollout restart deploy/dsp-internal deploy/dsp-competitor1 deploy/dsp-competitor2; \
	else kubectl -n adtech rollout restart deploy/$(SVC); fi
	kubectl -n adtech rollout status deploy/$(if $(filter dsp,$(SVC)),dsp-internal,$(SVC)) --timeout=180s

stack-down: ## Tear the local helm stack down (keeps PVCs; add PURGE=1 to wipe data)
	helm uninstall adtech || true
	@if [ "$(PURGE)" = "1" ]; then kubectl -n adtech delete pvc --all; fi

# ---- Staging / SOPS secrets (host-agnostic; needs sops + age + helm) ----
# The secret VALUES live in an encrypted overlay; only the .enc.yaml is committed
# (see .sops.yaml + docs/DEPLOY.md "Secrets"). ENV defaults to staging.
ENV ?= staging
SECRETS_DIR := k8s/helm/adtech/secrets

secrets-encrypt: ## Encrypt SECRETS_DIR/$(ENV).secrets.yaml → $(ENV).secrets.enc.yaml (commit only the .enc.yaml)
	@command -v sops >/dev/null || { echo "sops not installed — brew install sops age, then set your recipient in .sops.yaml"; exit 1; }
	sops -e $(SECRETS_DIR)/$(ENV).secrets.yaml > $(SECRETS_DIR)/$(ENV).secrets.enc.yaml
	@echo "encrypted → $(SECRETS_DIR)/$(ENV).secrets.enc.yaml (safe to commit)"

secrets-edit: ## Edit the encrypted $(ENV) secrets in place (sops opens $$EDITOR; re-encrypts on save)
	@command -v sops >/dev/null || { echo "sops not installed"; exit 1; }
	sops $(SECRETS_DIR)/$(ENV).secrets.enc.yaml

secrets-decrypt: ## Print the decrypted $(ENV) secrets to stdout (debug only; never redirect into git)
	@command -v sops >/dev/null || { echo "sops not installed"; exit 1; }
	sops -d $(SECRETS_DIR)/$(ENV).secrets.enc.yaml

deploy-staging: ## Deploy the chart to the CURRENT kubectl context: values-staging + SOPS secrets (host-agnostic). Prereqs: cert-manager + issuer installed, images pushed (build-push workflow), .sops.yaml recipient set + $(ENV).secrets.enc.yaml present.
	@command -v sops >/dev/null || { echo "sops not installed"; exit 1; }
	@[ -f $(SECRETS_DIR)/staging.secrets.enc.yaml ] || { echo "missing $(SECRETS_DIR)/staging.secrets.enc.yaml — copy the .example, fill it, 'make secrets-encrypt'"; exit 1; }
	@# bash for process substitution — the decrypted secrets never touch disk.
	bash -c 'helm upgrade --install adtech k8s/helm/adtech --timeout 10m \
	  -f k8s/helm/adtech/values-staging.yaml \
	  -f <(sops -d $(SECRETS_DIR)/staging.secrets.enc.yaml)'

stack-doctor: ## Diagnose + repair the local stack (post-sleep wedge, node-IP flip, stale svclb tunnels, TB wedge)
	scripts/stack-doctor.sh

# --- Utilities ---
clean: ## Clean build artifacts
	rm -rf bin/
	go clean -cache

tidy: ## Tidy Go modules
	go mod tidy

fmt: ## Format Go code
	go fmt ./...

# --- Help ---
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := help

bench: ## Hot-path micro-benchmarks (cluster-free): run + diff vs docs/perf/bench/baseline.txt via benchstat. Attributes a regression to a function+commit at PR time.
	bash scripts/bench.sh

bench-pin: ## Re-pin the micro-benchmark baseline (do deliberately after a known-good change; commit docs/perf/bench/baseline.txt in the same PR).
	bash scripts/bench.sh --pin

perfbench: ## Dated benchmark run(s): clean world per stage, ledger + raw Prometheus archive. RPS=110 DURATION=30m, or RPS_STAGES="20 50 80" for a sweep; PURGE=1 wipes PVCs first
	scripts/perfbench.sh

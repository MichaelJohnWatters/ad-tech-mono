.PHONY: setup proto test test-integration lint build seed simulate reset diagrams chaos help ssai-smoke

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

diagrams: ## Regenerate SVG diagrams from D2 source files
	@for f in docs/diagrams/*.d2; do \
		name=$$(basename "$$f" .d2); \
		echo "Rendering $$name..."; \
		d2 "$$f" "docs/diagrams/$${name}.svg"; \
	done
	@echo "Diagrams regenerated."

# --- Testing ---
test: ## Run unit tests
	go test ./pkg/... ./cmd/...

test-integration: ## Run integration tests (requires Docker for testcontainers)
	go test ./pkg/... ./cmd/... -tags=integration -count=1

test-duckdb: ## Run the CGO DuckDB analytics tests (requires a C toolchain)
	CGO_ENABLED=1 go test -tags=duckdb ./pkg/store/analytics/... ./cmd/reporting/... -count=1

test-clickhouse: ## Run the ClickHouse analytics integration tests (needs a live CH; tilt forwards :9010)
	CLICKHOUSE_ADDR=$${CLICKHOUSE_ADDR:-127.0.0.1:9010} go test -tags=clickhouse_integration ./pkg/store/analytics/... -count=1 -run ClickHouse

test-e2e: ## Run end-to-end tests on k3s (pins reporting to the memory backend — ADR 0001)
	./scripts/e2e-preflight.sh
	go test ./tests/... -tags=e2e -count=1 -timeout=10m

ssai-smoke: ## R1 live smoke: real stack conditions + serves a decodable ad segment (needs tilt up + ffmpeg)
	./scripts/ssai-smoke.sh

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

reset: ## Wipe database and reseed with standard profile
	go run ./cmd/migrate reset
	go run ./cmd/migrate
	go run ./cmd/seed --profile standard

# --- Performance Testing ---
perf-tracker: ## Run k6 load test on tracker
	k6 run tests/k6/tracker-load.js

perf-exchange: ## Run k6 load test on exchange
	k6 run tests/k6/exchange-load.js

perf-all: perf-tracker perf-exchange ## Run all k6 load tests

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

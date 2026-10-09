# lakesync developer workflow. Run `make help` for the list.
SHELL := /bin/bash
APP := lakesync
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X lakesync/internal/config.Version=$(VERSION) -X lakesync/internal/config.Commit=$(COMMIT)
COMPOSE := docker compose -f deploy/local/docker-compose.yml
export PATH := /opt/homebrew/bin:$(PATH)

.PHONY: help up down reset logs infra dev build ui test test-unit e2e e2e-container e2e-inner bench bench-container lint seed seed-host verify verify-host clean docker screenshots

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

up: ## Start the full local stack (infrastructure + lakesync) and open the UI
	$(COMPOSE) --profile app up -d --build --wait
	@echo "lakesync is running at http://localhost:8080 (login: admin@example.com / changeme-now)"

down: ## Stop the local stack (keeps volumes)
	$(COMPOSE) --profile app down

reset: ## Stop the local stack and delete all data volumes
	$(COMPOSE) --profile app down -v

logs: ## Tail lakesync logs
	$(COMPOSE) --profile app logs -f lakesync

infra: ## Start only the infrastructure (MongoDB, S3, Nessie, Postgres)
	$(COMPOSE) up -d --wait

dev: infra ## Run the Go server with live reload and the Vite dev server (proxying /api)
	@command -v air >/dev/null || go install github.com/air-verse/air@latest
	@set -a && source deploy/local/dev.env && set +a && \
	 (cd web && npm run dev -- --host 127.0.0.1 --port 5173 &) && \
	 air -c .air.toml

ui: ## Build the web UI into internal/api/static
	cd web && npm ci --no-audit --no-fund && npm run build
	rm -rf internal/api/static/* && cp -r web/dist/* internal/api/static/

build: ## Build the binary (requires `make ui` first for an embedded UI)
	CGO_ENABLED=1 go build -ldflags "$(LDFLAGS)" -o bin/$(APP) ./cmd/$(APP)

test: ## Unit and integration tests (integration needs `make infra`)
	go vet ./...
	go test ./... -count=1
	go test -tags integration ./... -count=1

test-unit: ## Unit tests only
	go test ./... -count=1

lint: ## golangci-lint (includes the cloud-neutral depguard check) and UI lint
	@command -v golangci-lint >/dev/null || (echo "install golangci-lint: brew install golangci-lint" && exit 1)
	golangci-lint run ./...
	cd web && npm run lint

e2e: ## Correctness, resync, source-removed, split-process, security and UI suites (needs `make infra`; builds the binary)
	$(MAKE) build
	go test -tags e2e ./test/e2e/... -count=1 -timeout 60m -v
	./scripts/ui-e2e.sh

e2e-container: ## Same suites, run from a container inside the compose network (no host port forwarding involved)
	$(COMPOSE) -f deploy/local/docker-compose.e2e.yml build e2e
	$(COMPOSE) -f deploy/local/docker-compose.e2e.yml run --rm e2e

e2e-inner: ## (used inside the e2e container) build a Linux binary, run the Go and UI suites
	go build -ldflags "$(LDFLAGS)" -o bin/lakesync-linux ./cmd/$(APP)
	go test -tags e2e ./test/e2e/... -count=1 -timeout 90m -v
	cd web && npm ci --no-audit --no-fund >/dev/null && cd .. && ./scripts/ui-e2e.sh

bench: ## Benchmarks for docs/BENCHMARKS.md (needs `make infra` and `make build`; LAKESYNC_BENCH_DOCS sets the table size)
	LAKESYNC_BENCH=1 go test -tags e2e ./test/e2e/ -run TestBenchmark -count=1 -timeout 4h -v

bench-container: ## Benchmarks from inside the compose network
	$(COMPOSE) -f deploy/local/docker-compose.e2e.yml build e2e
	LAKESYNC_BENCH=1 $(COMPOSE) -f deploy/local/docker-compose.e2e.yml run --rm e2e sh -c 'go build -o bin/lakesync-linux ./cmd/lakesync && go test -tags e2e ./test/e2e/ -run TestBenchmark -count=1 -timeout 4h -v'


seed: ## Generate test data in the local MongoDB through the app container (initial load + 5 minutes of changes)
	$(COMPOSE) --profile app run --rm --no-deps lakesync seed --uri "mongodb://mongodb:27017/?replicaSet=rs0" --duration 5m --new-field-after 1m --type-conflict-after 2m

seed-host: ## Same, but with a locally built binary against localhost:27017 (needs Go)
	go run ./cmd/$(APP) seed --duration 5m --new-field-after 1m --type-conflict-after 2m

verify: ## Compare every pipeline's collections with their clean tables (through the app container)
	$(COMPOSE) --profile app run --rm --no-deps lakesync verify

verify-host: ## Same with a local binary and deploy/local/dev.env (needs Go)
	@set -a && source deploy/local/dev.env && set +a && go run ./cmd/$(APP) verify

docker: ## Build the multi-arch image locally (loads the host arch)
	docker buildx build --platform linux/amd64,linux/arm64 -t $(APP):$(VERSION) -f Dockerfile .

screenshots: ## Capture UI screenshots into docs/screenshots (needs a running app)
	cd web && npx playwright test --project=screenshots

clean: ## Remove build output
	rm -rf bin web/dist internal/api/static/assets internal/api/static/index.html

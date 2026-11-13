# Common tasks. `make help` lists them. Requires Go 1.26+
# and uv (https://docs.astral.sh/uv/), which runs the SQL test runner and vgi-lint.
# managed-by: vgi-starters (language section below is go-specific)

HAYBARN_VERSION ?= 2.0.0rc11
# vgi-lint is deliberately not pinned: the gate tracks the current quality bar.
VGI_LINT        := uvx --prerelease=allow --from 'vgi-lint-check>=2,<3' vgi-lint lint
LINT_ARGS       ?=
GOLANGCI_LINT_VERSION ?= v2.14.0
EXE             := $(if $(filter Windows_NT,$(OS)),.exe,)
WORKER          := $(CURDIR)/bin/vgi-hue-worker$(EXE)
RECIPES         := $(CURDIR)/bin/recipes$(EXE)
TOOLS           := $(CURDIR)/.tools
HAYBARN_UNITTEST := $(TOOLS)/bin/haybarn-unittest
GOLANGCI_LINT   := $(TOOLS)/bin/golangci-lint$(EXE)
TRANSPORT       ?= stdio
# Pure Go by default; set CGO_ENABLED=1 if you import vgi-go/vgi/exprduckdb.
export CGO_ENABLED ?= 0

.DEFAULT_GOAL := help
.PHONY: help build test test-unit test-sql test-sql-all lint lint-explain lint-ai fmt check run run-http debug tools template-diff clean

help: ## List the targets
	@awk 'BEGIN{FS=":.*## "} /^[a-zA-Z_-]+:.*## /{printf "  %-14s %s\n",$$1,$$2}' $(MAKEFILE_LIST)


build: ## Build the worker and the recipes into bin/
	go build -o $(WORKER) ./cmd/vgi-hue-worker
	go build -o $(RECIPES) ./examples

tools: $(GOLANGCI_LINT) ## Install the pinned SQL test runner and golangci-lint into .tools
	@test -x $(HAYBARN_UNITTEST) || { uv venv -q --allow-existing $(TOOLS) && uv pip install -q -p $(TOOLS) haybarn-unittest==$(HAYBARN_VERSION); }

# Built from source with your Go toolchain, so it always understands the go.mod version.
$(GOLANGCI_LINT):
	GOBIN=$(TOOLS)/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

fmt: ## Format the code
	gofmt -w .

lint: $(GOLANGCI_LINT) ## Static checks: gofmt, go vet, golangci-lint
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	$(GOLANGCI_LINT) run ./...

test-unit: ## Unit tests (go test)
	go test ./...

test-sql: build tools ## SQL tests through DuckDB: make test-sql TRANSPORT=stdio|launch|http
	WORKER=$(WORKER) RECIPES_WORKER="$(RECIPES)" HAYBARN_UNITTEST=$(HAYBARN_UNITTEST) TRANSPORT=$(TRANSPORT) PYTHON="uv run --no-project python" ci/run-integration.sh

test-sql-all: ## SQL tests over every transport
	$(MAKE) test-sql TRANSPORT=stdio
	$(MAKE) test-sql TRANSPORT=launch
	$(MAKE) test-sql TRANSPORT=http

test: test-unit test-sql ## Unit + SQL tests

lint-explain: build ## Metadata quality report against the mock bridge (vgi-lint, settings in vgi-lint.toml). LINT_ARGS="--format agent" for agents
	PYTHON="uv run --no-project python" ci/lint.sh $(VGI_LINT) "$(WORKER)" $(LINT_ARGS)

lint-ai: ## Opt-in L3 check: an agent runs vgi-agent-tests.yaml and docs are reviewed (needs the claude CLI)
	@$(MAKE) --no-print-directory lint-explain LINT_ARGS="--ai $(LINT_ARGS)"

check: lint test-unit test-sql-all lint-explain ## Everything CI runs. Done = this passes.

run: build ## Serve over stdio (what DuckDB launches)
	$(WORKER)

run-http: build ## Serve over HTTP on port 8000
	$(WORKER) --http --host 127.0.0.1 --port 8000

debug: build tools ## Run one query against the worker with debug logging on stderr
	@printf "LOAD vgi;\nATTACH 'hue' (TYPE vgi, LOCATION '$(WORKER) --debug');\nSELECT hue.main.greet('debug');\n" \
	  | uvx --from haybarn-cli==$(HAYBARN_VERSION) haybarn -no-agent

template-diff: ## Show upstream template changes since this project was created
	uv run --no-project python scripts/vgi_template.py template-diff --stat

clean: ## Remove build outputs and tool caches
	rm -rf bin dist .tools

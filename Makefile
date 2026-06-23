# turntable — common build/dev tasks.
#
# `make build` produces the binary from the committed web UI bundle and needs no
# Node toolchain. `make ui` rebuilds that bundle (internal/cli/webui/dist) after
# changing the frontend; commit the result. `make all` does both.

BINARY := turntable
WEBUI  := internal/cli/webui
GO     := go
NPM    := npm

.DEFAULT_GOAL := help

## help: list available targets
.PHONY: help
help:
	@echo "turntable make targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'

## build: build the turntable binary (uses the committed UI bundle; no Node)
.PHONY: build
build:
	$(GO) build -o $(BINARY) ./cmd/turntable

## all: rebuild the web UI bundle, then the binary
.PHONY: all
all: ui build

## run: run the CLI (pass args via ARGS, e.g. make run ARGS='--repl')
.PHONY: run
run:
	$(GO) run ./cmd/turntable $(ARGS)

## serve: run the web UI (override addr/config via ARGS)
.PHONY: serve
serve:
	$(GO) run ./cmd/turntable --serve $(ARGS)

## test: run the pure-Go test suite
.PHONY: test
test:
	$(GO) test ./...

## test-integration: run sqlc tests against embedded Postgres + go-mysql-server
.PHONY: test-integration
test-integration:
	$(GO) test -tags integration ./internal/connector/connectors/sqlc/

## vet: run go vet
.PHONY: vet
vet:
	$(GO) vet ./...

## tidy: go mod tidy (keeps the integration-only deps — see CLAUDE.md)
.PHONY: tidy
tidy:
	GOFLAGS=-tags=integration $(GO) mod tidy

## examples: run the end-to-end demo queries
.PHONY: examples
examples:
	./examples/run.sh

# ---- web UI ------------------------------------------------------------------

# Install deps only when the lockfile changes (order-only via the directory).
$(WEBUI)/node_modules: $(WEBUI)/package-lock.json
	cd $(WEBUI) && $(NPM) ci
	@touch $(WEBUI)/node_modules

## ui: build the embedded web UI bundle (internal/cli/webui/dist)
.PHONY: ui
ui: $(WEBUI)/node_modules
	cd $(WEBUI) && $(NPM) run build

## ui-dev: run the Vite dev server (HMR on :5173, proxies /api to :8080)
.PHONY: ui-dev
ui-dev: $(WEBUI)/node_modules
	cd $(WEBUI) && $(NPM) run dev

## clean: remove the built binary
.PHONY: clean
clean:
	rm -f $(BINARY)

## clean-ui: remove web UI node_modules (dist stays; it is committed)
.PHONY: clean-ui
clean-ui:
	rm -rf $(WEBUI)/node_modules

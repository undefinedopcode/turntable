# turntable — common build/dev tasks.
#
# `make build` produces the binary from the committed web UI bundle and needs no
# Node toolchain. `make ui` rebuilds that bundle (internal/cli/webui/dist) after
# changing the frontend; commit the result. `make all` does both.

BINARY := turntable
WEBUI  := internal/cli/webui
GO     := go
NPM    := npm
PANDOC := pandoc

# Documentation rendering (DIALECT.md -> HTML/PDF via pandoc).
DOCS_DIR := docs
DOCS_CSS := $(DOCS_DIR)/style.css
# First available pandoc PDF engine, in preference order: LaTeX (xelatex/lualatex)
# for the most conventional typography, then the lighter typst / weasyprint;
# empty when none is present. Override on the command line to force one, e.g.
# `make docs PDF_ENGINE=typst`.
PDF_ENGINE ?= $(firstword $(foreach e,xelatex lualatex tectonic typst weasyprint wkhtmltopdf pdflatex prince,$(if $(shell command -v $(e) 2>/dev/null),$(e))))
# Fonts for the PDF body/code. pandoc's default fonts (typst: none; LaTeX: Latin
# Modern) are often absent, so we point at system fonts. Override if DejaVu is
# not installed: `make docs-pdf DOC_MAINFONT="Noto Sans" DOC_MONOFONT="Noto Sans Mono"`.
DOC_MAINFONT ?= DejaVu Sans
DOC_MONOFONT ?= DejaVu Sans Mono
# Only typst / xelatex / lualatex / tectonic honor -V mainfont (pdflatex can't use
# system fonts; weasyprint/wkhtmltopdf style via docs/style.css).
PDF_FONT_FLAGS := $(if $(filter $(PDF_ENGINE),typst xelatex lualatex tectonic),-V mainfont="$(DOC_MAINFONT)" -V monofont="$(DOC_MONOFONT)")

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

# ---- documentation -----------------------------------------------------------

## docs: render DIALECT.md to HTML (and PDF if a pandoc PDF engine is installed)
.PHONY: docs
docs: docs-html
	@if [ -n "$(PDF_ENGINE)" ]; then $(MAKE) docs-pdf; else \
	  echo "docs: skipping PDF — no pandoc PDF engine found."; \
	  echo "      install one to enable it, e.g.: pacman -S typst   (lightest)"; fi

## docs-html: render DIALECT.md to a standalone, styled docs/DIALECT.html
.PHONY: docs-html
docs-html: $(DOCS_DIR)/DIALECT.html

$(DOCS_DIR)/DIALECT.html: DIALECT.md $(DOCS_CSS)
	$(PANDOC) DIALECT.md \
	  --standalone --embed-resources --toc --toc-depth=3 \
	  --metadata title="Turntable SQL Dialect Reference" \
	  --highlight-style=tango --css $(DOCS_CSS) -o $@
	@echo "wrote $@"

## docs-pdf: render DIALECT.md to docs/DIALECT.pdf (needs a pandoc PDF engine)
# No --highlight-style: pandoc's default code highlighting has no background, so
# it avoids the `framed.sty` LaTeX package that background styles (e.g. tango)
# pull in — keeping the LaTeX engines working without extra texlive packages.
.PHONY: docs-pdf
docs-pdf: DIALECT.md
	@if [ -z "$(PDF_ENGINE)" ]; then \
	  echo "docs-pdf: no pandoc PDF engine found. Install one, e.g.:"; \
	  echo "  pacman -S typst              # lightest, recommended"; \
	  echo "  pacman -S python-weasyprint  # CSS-based, honors docs/style.css"; \
	  exit 1; fi
	$(PANDOC) DIALECT.md \
	  --toc --toc-depth=3 \
	  --metadata title="Turntable SQL Dialect Reference" \
	  --pdf-engine=$(PDF_ENGINE) $(PDF_FONT_FLAGS) \
	  -o $(DOCS_DIR)/DIALECT.pdf
	@echo "wrote $(DOCS_DIR)/DIALECT.pdf (engine: $(PDF_ENGINE), font: $(DOC_MAINFONT))"

## clean-docs: remove generated docs (keeps docs/style.css)
.PHONY: clean-docs
clean-docs:
	rm -f $(DOCS_DIR)/DIALECT.html $(DOCS_DIR)/DIALECT.pdf

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

# flotilla — Makefile
#
# Layered tier pattern:
#   - lint / format / test-unit / test-integration  → atomic, language-specific
#   - ci-fast                                       → what runs on every push (fast feedback)
#   - ci                                            → what runs before merge / release
#   - docs-*                                        → MkDocs site (preview + strict-build)
#
# Convention (per global AGENTS.md #17): each invocation should end with an
# unambiguous exit signal. Use:
#   make <target>; echo "MAKE_EXIT=$?"
# when running unattended.
#
# Atomic targets (lint, test-*) are intentionally minimal stubs. Fill them
# in once you've picked the language / framework. The composite tiers
# (ci-fast, ci) wire them up.

SHELL          := /usr/bin/env bash
.SHELLFLAGS    := -eu -o pipefail -c
.DEFAULT_GOAL  := help

# ---- Tunables ----------------------------------------------------------------

PYTHON         ?= python3
VENV_DIR       ?= .venv
VENV_BIN       := $(VENV_DIR)/bin
PIP            := $(VENV_BIN)/pip
MKDOCS         := $(VENV_BIN)/mkdocs

DOCS_REQS      := requirements-docs.txt
DOCS_PORT      ?= 8765
DOCS_BIND      ?= 127.0.0.1

# ---- Meta --------------------------------------------------------------------

.PHONY: help
help: ## Show this help (default target)
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} \
		/^[a-zA-Z0-9_.-]+:.*##/ {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' \
		$(MAKEFILE_LIST)

# ---- Atomic gates -------------------------------------------------------------
# fleetd (Go) and the triage fleet (Python, stdlib only) since phase 1. The bugfix fleet's
# TypeScript MVP has no lockfile and no CI yet (RFC-0001 §11 asks whether it is worth keeping).
# The replays need the private corpora, so they are `make replay`, run on a host, not in CI.

SF_MVP := signal-fleet/mvp

.PHONY: format
format: ## Apply gofmt to fleetd
	@cd fleetd && gofmt -w . && echo "format PASS"

.PHONY: lint
lint: ## gofmt check + go vet (fleetd), byte-compile every Python file (signal-fleet)
	@cd fleetd && test -z "$$(gofmt -l .)" || { echo "lint FAIL: gofmt would change:"; gofmt -l .; exit 1; }
	@cd fleetd && go vet ./...
	@python3 -m compileall -q signal-fleet >/dev/null || { echo "lint FAIL: Python does not compile"; exit 1; }
	@echo "lint PASS"

.PHONY: test-unit
test-unit: ## go test (fleetd) + the triage fleet's unit tests, with an empty HOME
	@cd fleetd && go test ./...
	@h=$$(mktemp -d); cd $(SF_MVP) && for t in test_units.py test_capture.py test_scrub.py; do \
	  HOME=$$h SF_OBSERV_DISABLED=1 python3 $$t >/tmp/flotilla-$$t.log 2>&1 && echo "PASS $$t" || { echo "FAIL $$t"; tail -20 /tmp/flotilla-$$t.log; rm -rf $$h; exit 1; }; \
	done; rm -rf $$h
	@echo "test-unit PASS"

.PHONY: replay
replay: ## The triage fleet's replay gates (needs SF_EVAL_DATA and SF_ALERT_RULES; see signal-fleet/replays.sh)
	@bash signal-fleet/replays.sh

.PHONY: test-integration
test-integration: ## Integration tests (real services) — none yet: no code
	@echo "test-integration: no code yet (design phase)"

# ---- Composite tiers ---------------------------------------------------------

.PHONY: ci-fast
ci-fast: lint test-unit ## What every push must pass (fast feedback, <2min ideal)
	@echo "ci-fast PASS"

.PHONY: ci
ci: ci-fast test-integration docs-build ## What every merge must pass (full gate)
	@echo "ci PASS"

# ---- Docs site (MkDocs Material) ---------------------------------------------

$(VENV_DIR):
	$(PYTHON) -m venv $(VENV_DIR)
	$(PIP) install --quiet --upgrade pip

.PHONY: docs-install
docs-install: $(VENV_DIR) ## Install/refresh docs dependencies into .venv
	$(PIP) install --quiet -r $(DOCS_REQS)
	@echo "docs deps installed in $(VENV_DIR)"

.PHONY: docs-serve
docs-serve: docs-install ## Run local docs site at http://$(DOCS_BIND):$(DOCS_PORT)
	$(MKDOCS) serve --dev-addr $(DOCS_BIND):$(DOCS_PORT)

.PHONY: docs-build
docs-build: docs-install ## Build static site into ./site (strict — fails on broken refs)
	$(MKDOCS) build --strict --clean
	@echo "docs built → ./site"

.PHONY: docs-validate
docs-validate: docs-build ## Alias for docs-build (strict catches the issues)
	@echo "docs validate OK"

.PHONY: docs-clean
docs-clean: ## Remove built site
	rm -rf site/
	@echo "site/ removed"

.PHONY: clean
clean: docs-clean ## Remove all build artifacts

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
# Design phase: the repo holds docs only, so there is nothing to format, lint or test yet. Phase 1
# (RFC-0001 §9) brings fleetd (Go) and the triage fleet (Python) and replaces these with gofmt /
# go vet / go test and ruff / pytest. Until then they say so and pass; they are not a skipped check.

.PHONY: format
format: ## Apply formatters (gofmt, ruff format) — none yet: no code
	@echo "format: no code yet (design phase)"

.PHONY: lint
lint: ## Run linters (go vet, ruff) — none yet: no code
	@echo "lint: no code yet (design phase)"

.PHONY: test-unit
test-unit: ## Fast unit tests (go test, pytest) — none yet: no code
	@echo "test-unit: no code yet (design phase)"

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

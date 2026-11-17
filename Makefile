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
# Go: flotillad and the CLI (the root module) and fleetd (its own module, until it retires). Python:
# the worker and the triage fleet (stdlib only). The bugfix fleet's TypeScript MVP has no lockfile
# and no CI yet (RFC-0001 §11). The replays need the private corpora, so they are `make replay`, run
# on a host, not in CI. test-integration and test-e2e are one script, run by .github/workflows/e2e.yml.
# Without Go installed: make GO=scripts/go.sh GOFMT="scripts/go.sh gofmt" ...

SF_MVP := signal-fleet/mvp
GO     ?= go
GOFMT  ?= gofmt

.PHONY: format
format: ## Apply gofmt to every Go file
	@$(GOFMT) -w cmd internal fleetd && echo "format PASS"

.PHONY: lint
lint: ## gofmt check + go vet (both modules), byte-compile every Python file, llms.txt up to date, no installation facts
	@out="$$($(GOFMT) -l cmd internal fleetd)"; test -z "$$out" || { echo "lint FAIL: gofmt would change:"; echo "$$out"; exit 1; }
	@$(GO) vet ./...
	@cd fleetd && $(if $(filter scripts/%,$(GO)),../$(GO),$(GO)) vet ./...
	@python3 -m compileall -q signal-fleet worker deploy runtime >/dev/null || { echo "lint FAIL: Python does not compile"; exit 1; }
	@python3 scripts/llms_txt.py --check >/dev/null || { echo "lint FAIL: llms.txt"; python3 scripts/llms_txt.py --check; exit 1; }
	@python3 scripts/installation_facts.py >/tmp/flotilla-installation-facts.log 2>&1 || { echo "lint FAIL: installation facts in the code (vision plan step 7):"; cat /tmp/flotilla-installation-facts.log; exit 1; }
	@echo "lint PASS"

.PHONY: llms-txt
llms-txt: ## Regenerate llms.txt, the agent-facing index of the docs (scripts/llms_txt.py, llms.config.json)
	@python3 scripts/llms_txt.py --write

.PHONY: test-unit
test-unit: ## go test (both modules), the worker's tests, the triage fleet's unit tests (empty HOME)
	@$(GO) test ./...
	@cd fleetd && $(if $(filter scripts/%,$(GO)),../$(GO),$(GO)) test ./...
	@python3 worker/test_worker.py >/tmp/flotilla-test_worker.log 2>&1 && echo "PASS worker/test_worker.py" || { echo "FAIL worker/test_worker.py"; tail -30 /tmp/flotilla-test_worker.log; exit 1; }
	@python3 ops/test_ops.py >/tmp/flotilla-test_ops.log 2>&1 && echo "PASS ops/test_ops.py" || { echo "FAIL ops/test_ops.py"; tail -30 /tmp/flotilla-test_ops.log; exit 1; }
	@python3 ops/test_case_review.py >/tmp/flotilla-test_case_review.log 2>&1 && echo "PASS ops/test_case_review.py" || { echo "FAIL ops/test_case_review.py"; tail -30 /tmp/flotilla-test_case_review.log; exit 1; }
	@python3 bugfix/test_bugfix.py >/tmp/flotilla-test_bugfix.log 2>&1 && echo "PASS bugfix/test_bugfix.py" || { echo "FAIL bugfix/test_bugfix.py"; tail -30 /tmp/flotilla-test_bugfix.log; exit 1; }
	@python3 deps/test_deps.py >/tmp/flotilla-test_deps.log 2>&1 && echo "PASS deps/test_deps.py" || { echo "FAIL deps/test_deps.py"; tail -30 /tmp/flotilla-test_deps.log; exit 1; }
	@python3 business/test_business.py >/tmp/flotilla-test_business.log 2>&1 && echo "PASS business/test_business.py" || { echo "FAIL business/test_business.py"; tail -30 /tmp/flotilla-test_business.log; exit 1; }
	@timeout 300 python3 bugfix/test_fix.py >/tmp/flotilla-test_fix.log 2>&1 && echo "PASS bugfix/test_fix.py" || { echo "FAIL bugfix/test_fix.py"; tail -30 /tmp/flotilla-test_fix.log; exit 1; }
	@timeout 240 python3 bugfix/test_spend.py >/tmp/flotilla-test_spend.log 2>&1 && echo "PASS bugfix/test_spend.py" || { echo "FAIL bugfix/test_spend.py"; tail -30 /tmp/flotilla-test_spend.log; exit 1; }
	@python3 bugfix/test_callrec_pi.py >/tmp/flotilla-test_callrec_pi.log 2>&1 && echo "PASS bugfix/test_callrec_pi.py" || { echo "FAIL bugfix/test_callrec_pi.py"; tail -30 /tmp/flotilla-test_callrec_pi.log; exit 1; }
	@timeout 120 python3 bugfix/test_stepone.py >/tmp/flotilla-test_stepone.log 2>&1 && echo "PASS bugfix/test_stepone.py" || { echo "FAIL bugfix/test_stepone.py"; tail -30 /tmp/flotilla-test_stepone.log; exit 1; }
	@python3 seo/test_seo.py >/tmp/flotilla-test_seo.log 2>&1 && echo "PASS seo/test_seo.py" || { echo "FAIL seo/test_seo.py"; tail -30 /tmp/flotilla-test_seo.log; exit 1; }
	@python3 seo/test_sources.py >/tmp/flotilla-test_sources.log 2>&1 && echo "PASS seo/test_sources.py" || { echo "FAIL seo/test_sources.py"; tail -30 /tmp/flotilla-test_sources.log; exit 1; }
	@timeout 300 python3 bugfix/test_open_pr.py >/tmp/flotilla-test_open_pr.log 2>&1 && echo "PASS bugfix/test_open_pr.py" || { echo "FAIL bugfix/test_open_pr.py"; tail -30 /tmp/flotilla-test_open_pr.log; exit 1; }
	@python3 deploy/mini/test_mirror.py >/tmp/flotilla-test_mirror.log 2>&1 && echo "PASS deploy/mini/test_mirror.py" || { echo "FAIL deploy/mini/test_mirror.py"; tail -30 /tmp/flotilla-test_mirror.log; exit 1; }
	@python3 deploy/mini/test_shadow_compare.py >/tmp/flotilla-test_shadow_compare.log 2>&1 && echo "PASS deploy/mini/test_shadow_compare.py" || { echo "FAIL deploy/mini/test_shadow_compare.py"; tail -30 /tmp/flotilla-test_shadow_compare.log; exit 1; }
	@python3 deploy/mini/test_triage_hooks.py >/tmp/flotilla-test_triage_hooks.log 2>&1 && echo "PASS deploy/mini/test_triage_hooks.py" || { echo "FAIL deploy/mini/test_triage_hooks.py"; tail -30 /tmp/flotilla-test_triage_hooks.log; exit 1; }
	@python3 deploy/mini/test_deploy_store.py >/tmp/flotilla-test_deploy_store.log 2>&1 && echo "PASS deploy/mini/test_deploy_store.py" || { echo "FAIL deploy/mini/test_deploy_store.py"; tail -30 /tmp/flotilla-test_deploy_store.log; exit 1; }
	@python3 deploy/mini/test_checks_green.py >/tmp/flotilla-test_checks_green.log 2>&1 && echo "PASS deploy/mini/test_checks_green.py" || { echo "FAIL deploy/mini/test_checks_green.py"; tail -30 /tmp/flotilla-test_checks_green.log; exit 1; }
	@h=$$(mktemp -d); HOME=$$h python3 runtime/test_model_client.py >/tmp/flotilla-test_model_client.log 2>&1 && echo "PASS runtime/test_model_client.py" || { echo "FAIL runtime/test_model_client.py"; tail -30 /tmp/flotilla-test_model_client.log; rm -rf $$h; exit 1; }; rm -rf $$h
	@h=$$(mktemp -d); HOME=$$h python3 runtime/test_contract_routine.py >/tmp/flotilla-test_contract_routine.log 2>&1 && echo "PASS runtime/test_contract_routine.py" || { echo "FAIL runtime/test_contract_routine.py"; tail -30 /tmp/flotilla-test_contract_routine.log; rm -rf $$h; exit 1; }; rm -rf $$h
	@h=$$(mktemp -d); HOME=$$h python3 runtime/test_loaded.py >/tmp/flotilla-test_loaded.log 2>&1 && echo "PASS runtime/test_loaded.py" || { echo "FAIL runtime/test_loaded.py"; tail -30 /tmp/flotilla-test_loaded.log; rm -rf $$h; exit 1; }; rm -rf $$h
	@python3 runtime/test_skills.py >/tmp/flotilla-test_skills.log 2>&1 && echo "PASS runtime/test_skills.py" || { echo "FAIL runtime/test_skills.py"; tail -30 /tmp/flotilla-test_skills.log; exit 1; }
	@python3 runtime/test_toolrec.py >/tmp/flotilla-test_toolrec.log 2>&1 && echo "PASS runtime/test_toolrec.py" || { echo "FAIL runtime/test_toolrec.py"; tail -30 /tmp/flotilla-test_toolrec.log; exit 1; }
	@python3 runtime/test_harness.py >/tmp/flotilla-test_harness.log 2>&1 && echo "PASS runtime/test_harness.py" || { echo "FAIL runtime/test_harness.py"; tail -30 /tmp/flotilla-test_harness.log; exit 1; }
	@python3 runtime/test_ledger.py >/tmp/flotilla-test_ledger.log 2>&1 && echo "PASS runtime/test_ledger.py" || { echo "FAIL runtime/test_ledger.py"; tail -30 /tmp/flotilla-test_ledger.log; exit 1; }
	@h=$$(mktemp -d); cd $(SF_MVP) && for t in test_units.py test_capture.py test_scrub.py test_replay_api.py test_dev_filter.py test_probes.py; do \
	  HOME=$$h SF_OBSERV_DISABLED=1 python3 $$t >/tmp/flotilla-$$t.log 2>&1 && echo "PASS $$t" || { echo "FAIL $$t"; tail -20 /tmp/flotilla-$$t.log; rm -rf $$h; exit 1; }; \
	done; rm -rf $$h
	@echo "test-unit PASS"

.PHONY: replay
replay: ## The installation's triage replay gates, from its private fleets repo (SF_REPLAYS: its triage/gates/run.sh; SF_ALERT_RULES)
	@test -n "$${SF_REPLAYS:-}" || { echo "REPLAYS FAIL: SF_REPLAYS is not set (the private fleets repo's triage/gates/run.sh)"; exit 1; }
	@SF_FLOTILLA_MVP=$(CURDIR)/$(SF_MVP) bash "$$SF_REPLAYS"

.PHONY: test-e2e
test-e2e: ## The compose stack on the dev Docker engine with stand-in services (deploy/e2e/run.sh)
	@bash deploy/e2e/run.sh

.PHONY: test-integration
test-integration: test-e2e ## Integration = the e2e stack (flotillad, worker, proxy together)

# ---- Composite tiers ---------------------------------------------------------

.PHONY: ci-fast
ci-fast: lint test-unit ## What every push must pass (fast feedback, <2min ideal)
	@echo "ci-fast PASS"

.PHONY: ci
ci: ci-fast test-integration docs-lint docs-build ## What every merge must pass (full gate)
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

# markdownlint-cli, pinned; a local install of the same version is used when present.
MARKDOWNLINT ?= $(shell markdownlint --version 2>/dev/null | grep -qx 0.50.0 && echo markdownlint || echo npx --yes markdownlint-cli@0.50.0)

.PHONY: docs-lint
docs-lint: ## Lint every Markdown file (.markdownlint.json; .markdownlintignore skips imported fleet code)
	@env -u NODE_OPTIONS $(MARKDOWNLINT) . && echo "docs-lint PASS" || { echo "docs-lint FAIL (make docs-lint-fix fixes most)"; exit 1; }

.PHONY: docs-lint-fix
docs-lint-fix: ## Apply markdownlint's own fixes; long lines are left to fix by hand
	@env -u NODE_OPTIONS $(MARKDOWNLINT) --fix . || true
	@$(MAKE) --no-print-directory docs-lint

.PHONY: docs-validate
docs-validate: docs-lint docs-build ## Markdown lint plus the strict build
	@echo "docs validate OK"

.PHONY: docs-clean
docs-clean: ## Remove built site
	rm -rf site/
	@echo "site/ removed"

.PHONY: clean
clean: docs-clean ## Remove all build artifacts

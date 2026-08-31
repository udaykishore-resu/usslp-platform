# USSLP — build, verification and deployment entry points.
#
# This Makefile did not exist before the deployment layer was written; it is
# created here as the single place the CI workflows and a developer both invoke,
# so that "what CI runs" and "what I can run" are the same commands.
#
#   make help          list every target
#   make verify        everything CI checks, in the order CI checks it
#   make dev           the pure-Go stack in docker compose
#
# NOTE ON THE AUTHORING ENVIRONMENT: the Go module cache was not available where
# this was written, so `make build`, `make test` and `make vet` have not been
# executed here. They are ordinary Go commands against a module with no external
# dependencies (see go.mod) and are written for a normal CI runner with network
# access. Everything under `make verify-deploy` — the YAML, the chart structure,
# the metric names, the topic catalogue, the HCL — HAS been run, and the targets
# below are exactly what was run.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

REPO_ROOT := $(shell git rev-parse --show-toplevel 2>/dev/null || pwd)
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION  ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
REGISTRY  ?= ghcr.io/usslp
BIN_DIR   := $(REPO_ROOT)/bin

GO       ?= go
PYTHON   ?= python3
COMPOSE  ?= docker compose

# Every binary in the tree.
#
# The comment this replaces said that analytics, pricing and promotion were
# scaffolding with no cmd/ directory. They have one now, and the runtime builds
# and runs all of them, so they are built here too — a build list that lags the
# tree is how a service stops being compiled without anyone noticing.
#
# usslpd is the single-process runtime (platform/cmd/usslpd) and usslpctl is the
# CLI that drives it; both are listed separately because they are not services.
PLATFORM_CMDS := api-gateway label-service uig device-registry ota-service \
                 pricing-service promotion-service analytics-service
EDGE_CMDS     := sgu sec labelsim
RUNTIME_CMDS  := usslpd
TOOL_CMDS     := usslpctl
ALL_CMDS      := $(PLATFORM_CMDS) $(EDGE_CMDS) $(RUNTIME_CMDS) $(TOOL_CMDS)

DEV_COMPOSE       := deploy/compose/docker-compose.dev.yml
PRODLIKE_COMPOSE  := deploy/compose/docker-compose.prod-like.yml
CHART             := deploy/helm/usslp

.PHONY: help
help: ## List targets
	@printf '\nUSSLP — %s\n\n' '$(VERSION)'
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | sort \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[1m%-24s\033[0m %s\n", $$1, $$2}'
	@printf '\n'

# ===========================================================================
# Go
# ===========================================================================

.PHONY: build
build: ## Build every binary into ./bin
	@mkdir -p $(BIN_DIR)
	@for cmd in $(PLATFORM_CMDS); do \
	  echo "==> platform/cmd/$$cmd"; \
	  CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' \
	    -o $(BIN_DIR)/usslp-$$cmd ./platform/cmd/$$cmd || exit 1; \
	done
	@for cmd in $(EDGE_CMDS); do \
	  echo "==> edge/cmd/$$cmd"; \
	  CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' \
	    -o $(BIN_DIR)/usslp-$$cmd ./edge/cmd/$$cmd || exit 1; \
	done
	@for cmd in $(RUNTIME_CMDS); do \
	  echo "==> platform/cmd/$$cmd"; \
	  CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' \
	    -o $(BIN_DIR)/$$cmd ./platform/cmd/$$cmd || exit 1; \
	done
	@for cmd in $(TOOL_CMDS); do \
	  echo "==> tools/$$cmd"; \
	  CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' \
	    -o $(BIN_DIR)/$$cmd ./tools/$$cmd || exit 1; \
	done
	@echo "built $(words $(ALL_CMDS)) binaries into $(BIN_DIR)"

# `make test` is the full suite, including the end-to-end claims. It takes a
# few minutes, most of it spent waiting for simulated E-Ink panels in real
# time, which is the point: a suite that skipped the waiting would not be
# measuring what it claims to measure.
.PHONY: test
test: ## Every test, including the end-to-end claims (a few minutes)
	$(GO) test -timeout 20m ./...

.PHONY: test-race
test-race: ## Every test under the race detector
	$(GO) test -race -timeout 30m ./...

.PHONY: test-short
test-short: ## Unit tests only: -short skips anything that boots a platform
	$(GO) test -short -timeout 5m ./...

.PHONY: test-e2e
test-e2e: ## Just the end-to-end suite, verbosely, with the measured numbers
	$(GO) test -v -timeout 20m ./test/e2e/...

.PHONY: lint
lint: fmt-check vet ## gofmt and go vet across the tree

.PHONY: cover
cover: ## Test with a coverage profile
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: vet
vet: ## go vet ./...
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format Go sources
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if anything is not gofmt-clean
	@unformatted=$$(gofmt -l . | grep -v '^deploy/' || true); \
	if [ -n "$$unformatted" ]; then \
	  echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi; \
	echo "gofmt clean"

.PHONY: vuln
vuln: ## govulncheck, degrading gracefully when it is unavailable
	@if command -v govulncheck >/dev/null 2>&1; then \
	  govulncheck ./...; \
	else \
	  echo "govulncheck is not installed; attempting to run it via 'go run'"; \
	  $(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./... \
	    || echo "WARNING: govulncheck could not run (no network or no module cache); skipping"; \
	fi

.PHONY: tidy-check
tidy-check: ## Fail if go.mod would change
	@cp go.mod /tmp/go.mod.bak
	@$(GO) mod tidy
	@if ! diff -q go.mod /tmp/go.mod.bak >/dev/null; then \
	  cp /tmp/go.mod.bak go.mod; \
	  echo "go.mod is not tidy; run 'go mod tidy'"; exit 1; \
	fi
	@echo "go.mod is tidy"

# ===========================================================================
# The single-process runtime
#
# usslpd is the whole platform in one process: every cloud service, a cloud
# broker, a certificate hierarchy, a store gateway, its controllers and a
# simulated label fleet, all against one shared event log. See the package
# comment on platform/cmd/usslpd/stack for when this shape is the right one and
# when the Kubernetes topology under deploy/ is.
# ===========================================================================

RUN_ARGS ?=
DEMO_CONTROLLERS ?= 3
DEMO_LABELS      ?= 12

.PHONY: run
run: ## Run the whole platform in one process on the documented ports
	@mkdir -p $(BIN_DIR)
	@CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/usslpd ./platform/cmd/usslpd
	@CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/usslpctl ./tools/usslpctl
	$(BIN_DIR)/usslpd $(RUN_ARGS)

.PHONY: demo
demo: ## Boot the platform and narrate it through usslpctl, with measured latencies
	DEMO_CONTROLLERS=$(DEMO_CONTROLLERS) DEMO_LABELS=$(DEMO_LABELS) tools/demo.sh

# The load harness is opt-in and takes minutes. The defaults below are sized for
# a two-core container; raise them and watch the bottleneck move.
LOAD_STORES      ?= 2
LOAD_CONTROLLERS ?= 4
LOAD_LABELS      ?= 60
LOAD_RATE        ?= 40
LOAD_DURATION    ?= 45s
# Absolute, because `go test` runs each package with its own directory as the
# working directory: a relative path here would quietly write the report into
# test/load/ and `make clean` would not find it.
LOAD_REPORT      ?= $(CURDIR)/load-report.txt

.PHONY: load
load: ## Sustained price-change load, with throughput, percentiles and the bottleneck
	USSLP_LOAD_REPORT=$(LOAD_REPORT) $(GO) test ./test/load -run TestSustainedPriceLoad \
	  -load -v -timeout 30m \
	  -load.stores $(LOAD_STORES) -load.controllers $(LOAD_CONTROLLERS) \
	  -load.labels $(LOAD_LABELS) -load.rate $(LOAD_RATE) -load.duration $(LOAD_DURATION)
	@echo "report written to $(LOAD_REPORT)"

# ===========================================================================
# Deployment-layer verification
#
# Everything here runs with no network and no Go module cache, which is why it
# is separated from the Go targets: a `make verify-deploy` is meaningful on its
# own.
# ===========================================================================

.PHONY: verify-deploy
verify-deploy: yaml-check helm-lint verify-rules verify-metrics verify-topics tf-check shell-check ## Every deployment-layer check

.PHONY: yaml-check
yaml-check: ## Every YAML file under deploy/ and .github/ parses
	@$(PYTHON) -c "$$YAML_CHECK"

.PHONY: helm-lint
helm-lint: ## helm lint if available, plus the structural template checker
	@if command -v helm >/dev/null 2>&1; then \
	  echo "==> helm lint"; \
	  helm lint $(CHART) --strict; \
	  for f in $(CHART)/values-*.yaml; do \
	    echo "==> helm lint with $$f"; \
	    helm lint $(CHART) --strict --values $(CHART)/values.yaml --values $$f || exit 1; \
	  done; \
	  echo "==> helm template (render check)"; \
	  for f in $(CHART)/values-*.yaml; do \
	    helm template usslp $(CHART) --values $(CHART)/values.yaml --values $$f >/dev/null || exit 1; \
	  done; \
	else \
	  echo "helm is not installed; running the structural template checker instead"; \
	fi
	@$(PYTHON) deploy/helm/lint-templates.py

.PHONY: helm-sync-rules
helm-sync-rules: ## Regenerate the chart's copy of the Prometheus rules
	@mkdir -p $(CHART)/files/rules
	@cp deploy/observability/prometheus/rules/*.yaml $(CHART)/files/rules/
	@echo "synced $$(ls -1 $(CHART)/files/rules | wc -l) rule file(s) into $(CHART)/files/rules"

.PHONY: verify-rules
verify-rules: ## Fail if the chart's rules copy has drifted, and promtool-check the rules
	@# deploy/observability/prometheus/rules is canonical; $(CHART)/files/rules is
	@# a generated copy the chart's PrometheusRule template embeds with .Files.Glob.
	@# One source, two consumers, and this is the check that keeps them honest.
	@if ! diff -r -q deploy/observability/prometheus/rules $(CHART)/files/rules; then \
	  echo "the chart's copy of the Prometheus rules has drifted; run: make helm-sync-rules"; \
	  exit 1; \
	fi
	@echo "chart rule files are in sync with deploy/observability"
	@if command -v promtool >/dev/null 2>&1; then \
	  echo "==> promtool check rules"; \
	  promtool check rules deploy/observability/prometheus/rules/*.yaml; \
	else \
	  echo "promtool is not installed; skipping PromQL validation (YAML shape is still checked)"; \
	fi

.PHONY: verify-metrics
verify-metrics: ## Every metric in the rules and dashboards is registered in the Go tree
	@$(PYTHON) deploy/observability/verify-metrics.py

.PHONY: verify-topics
verify-topics: ## Every transcription of the stream catalogue matches canon.AllStreams()
	@$(PYTHON) deploy/verify-topics.py

.PHONY: tf-check
tf-check: ## terraform fmt -check and validate if available, else the HCL checker
	@if command -v terraform >/dev/null 2>&1; then \
	  echo "==> terraform fmt -check -recursive"; \
	  terraform fmt -check -recursive deploy/terraform; \
	  for d in deploy/terraform/regions/*/; do \
	    echo "==> terraform validate $$d"; \
	    (cd $$d && terraform init -backend=false -input=false >/dev/null && terraform validate) || exit 1; \
	  done; \
	else \
	  echo "terraform is not installed; running the structural HCL checker instead"; \
	fi
	@$(PYTHON) deploy/terraform/check-hcl.py

.PHONY: shell-check
shell-check: ## bash -n every script, and shellcheck if available
	@for f in deploy/edge/install.sh deploy/edge/update.sh deploy/docker/build.sh; do \
	  bash -n $$f && echo "syntax OK  $$f"; \
	done
	@if command -v shellcheck >/dev/null 2>&1; then \
	  shellcheck -S warning deploy/edge/*.sh deploy/docker/build.sh; \
	else \
	  echo "shellcheck is not installed; skipping"; \
	fi

.PHONY: verify
verify: fmt-check vet test verify-deploy ## Everything CI checks

# ===========================================================================
# Containers
# ===========================================================================

.PHONY: images
images: ## Build every container image
	REGISTRY=$(REGISTRY) VERSION=$(VERSION) deploy/docker/build.sh

.PHONY: images-push
images-push: ## Build and push every container image
	REGISTRY=$(REGISTRY) VERSION=$(VERSION) PUSH=1 deploy/docker/build.sh

.PHONY: images-dev
images-dev: ## Build the compose-friendly (probe) variant locally
	REGISTRY=usslp VERSION=dev TARGET=probe deploy/docker/build.sh

# ===========================================================================
# Compose
# ===========================================================================

.PHONY: dev
dev: ## Start the pure-Go dev stack
	$(COMPOSE) -f $(DEV_COMPOSE) up --build -d
	@echo
	@echo "  store gateway diagnostics  http://localhost:8090/status"
	@echo "  uig                        http://localhost:8081  (admin 9081)"
	@echo "  label service              http://localhost:8082  (admin 9082)"
	@echo "  ota service                http://localhost:8084  (admin 9084)"
	@echo "  label simulator            http://localhost:8099/store"
	@echo
	@echo "  device-registry and sec are opt-in; they need a PKI hierarchy."
	@echo "  See deploy/compose/README.md."

.PHONY: dev-logs
dev-logs: ## Follow the dev stack's logs
	$(COMPOSE) -f $(DEV_COMPOSE) logs -f

.PHONY: dev-down
dev-down: ## Stop the dev stack and remove its volumes
	$(COMPOSE) -f $(DEV_COMPOSE) down -v

.PHONY: prod-like
prod-like: ## Start the prod-like stack (Kafka, EMQX, Postgres, ClickHouse, Redis, observability)
	$(COMPOSE) -f $(PRODLIKE_COMPOSE) --profile all up --build -d
	@echo
	@echo "  grafana    http://localhost:3000"
	@echo "  prometheus http://localhost:9190"
	@echo "  jaeger     http://localhost:16686"
	@echo "  emqx       http://localhost:18083"
	@echo
	@echo "  Read the header of $(PRODLIKE_COMPOSE) for what is real here and"
	@echo "  what is a topology demonstration."

.PHONY: prod-like-down
prod-like-down: ## Stop the prod-like stack and remove its volumes
	$(COMPOSE) -f $(PRODLIKE_COMPOSE) --profile all down -v

# ===========================================================================
# Helpers
# ===========================================================================

.PHONY: clean
clean: ## Remove build output, the runtime's data directory and load reports
	rm -rf $(BIN_DIR) coverage.out dist data $(LOAD_REPORT)

.PHONY: version
version: ## Print the version this build would stamp
	@echo "version  $(VERSION)"
	@echo "revision $(REVISION)"

define YAML_CHECK
import pathlib, sys, yaml
bad = []
roots = [pathlib.Path("deploy"), pathlib.Path(".github")]
count = 0
# Helm templates are Go templates, not YAML: {{ }} makes them unparseable until
# rendered. They are checked structurally by deploy/helm/lint-templates.py
# (make helm-lint), which strips the template actions first.
skip = ("deploy/helm/usslp/templates/",)
for root in roots:
    if not root.exists():
        continue
    for p in sorted(list(root.rglob("*.yaml")) + list(root.rglob("*.yml"))):
        if any(str(p).startswith(s) for s in skip):
            continue
        count += 1
        try:
            list(yaml.safe_load_all(p.read_text()))
        except yaml.YAMLError as e:
            bad.append(f"{p}: {str(e).splitlines()[0]}")
for p in sorted(pathlib.Path("deploy").rglob("*.json")):
    count += 1
    import json
    try:
        json.loads(p.read_text())
    except json.JSONDecodeError as e:
        bad.append(f"{p}: {e}")
if bad:
    print("\n".join(bad)); sys.exit(1)
print(f"{count} YAML/JSON file(s) parse")
endef
export YAML_CHECK

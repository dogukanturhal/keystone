# Keystone — build, generate, and verify targets
# SPDX-License-Identifier: AGPL-3.0-or-later

SHELL          := /usr/bin/env bash

# Force module-mode without workspace. See docs/go-workspace-note.md for the
# rationale: legacy monorepo services pin a 2020-era genproto layout that
# conflicts with controller-runtime's modern split. Keystone is a
# self-contained module and must stay buildable without go.work.
export GOWORK   := off

GO             ?= go
PKG            := github.com/dogukanturhal/keystone
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT         ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS        := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(BUILD_DATE)

BIN_DIR        := bin
MANAGER_BIN    := $(BIN_DIR)/keystone-manager

# Tool versions — pinned for reproducible builds
CONTROLLER_GEN_VERSION ?= v0.20.1
KUSTOMIZE_VERSION      ?= v5.4.3
ENVTEST_VERSION        ?= release-0.19
ENVTEST_K8S_VERSION    ?= 1.31.0
GOLANGCI_LINT_VERSION  ?= v1.61.0
CRD_REF_DOCS_VERSION   ?= v0.2.0

# Local tool installation (pinned binaries under bin/)
LOCALBIN       := $(abspath $(BIN_DIR))
CONTROLLER_GEN := $(LOCALBIN)/controller-gen
KUSTOMIZE      := $(LOCALBIN)/kustomize
ENVTEST        := $(LOCALBIN)/setup-envtest
GOLANGCI_LINT  := $(LOCALBIN)/golangci-lint
CRD_REF_DOCS   := $(LOCALBIN)/crd-ref-docs

.PHONY: help
help: ## Show available targets
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

##@ Build

.PHONY: build
build: generate $(MANAGER_BIN) ## Build the manager binary

$(MANAGER_BIN): $(shell find . -name '*.go' -not -path './vendor/*' -not -path './bin/*' 2>/dev/null)
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags="$(LDFLAGS)" -o $(MANAGER_BIN) ./cmd/manager

##@ Generate

.PHONY: generate
generate: controller-gen ## Generate deepcopy methods + CRD manifests
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."
	$(CONTROLLER_GEN) rbac:roleName=keystone-manager-role \
		crd:allowDangerousTypes=true \
		webhook \
		paths="./..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

.PHONY: manifests
manifests: generate ## Alias for generate (kubebuilder convention)

.PHONY: api-reference
api-reference: crd-ref-docs ## Regenerate docs/api-reference.md from api/v1alpha1 Go types
	$(CRD_REF_DOCS) \
		--source-path=./api/v1alpha1 \
		--config=hack/crd-ref-config.yaml \
		--renderer=markdown \
		--output-path=docs/api-reference.md
	@echo "docs/api-reference.md regenerated — review diff before committing"

##@ Test

.PHONY: test
test: generate envtest ## Run unit + integration tests
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		$(GO) test -race -count=1 -coverprofile=cover.out ./...

.PHONY: lint
lint: golangci-lint ## Run linters
	$(GOLANGCI_LINT) run ./...

.PHONY: tidy
tidy: ## Tidy go modules
	$(GO) mod tidy

# Keystone's schema vocabulary lives in three modules that must move in
# order: keystone/api -> keystone-sdk/go -> keystone. Tagging each one to
# unblock the next would make every API addition a three-release ceremony.
# Go's answer is the pseudo-version: `@<branch>` resolves an untagged but
# pushed commit to a canonical vX.Y.Z-0.yyyymmddhhmmss-abcdefabcdef, which
# is a real version the proxy serves — so each repo's MR goes green as
# soon as its dependency is MERGED, and tags stay a release-time concern.
# See docs/release-train.md.
.PHONY: sync-sdk
sync-sdk: ## Repin keystone-sdk/go to its latest pushed commit (pseudo-version)
	$(GO) get github.com/dogukanturhal/keystone-sdk/go@$(SDK_REF)
	$(GO) mod tidy
	$(GO) build ./...
	@echo "pinned: $$($(GO) list -m github.com/dogukanturhal/keystone-sdk/go)"

# Branch or commit to pin against. Override for a release tag:
#   make sync-sdk SDK_REF=go/v0.3.0
SDK_REF        ?= main

.PHONY: verify
verify: tidy generate api-reference lint sast test policy-test verify-api-reference ## Run all verification gates

# verify-api-reference catches the "forgot to regenerate" MR. Runs after
# api-reference so any drift is visible as a non-zero git status.
.PHONY: verify-api-reference
verify-api-reference: ## Fail if docs/api-reference.md is out-of-date vs the current api/v1alpha1
	@if ! git diff --quiet -- docs/api-reference.md 2>/dev/null; then \
		echo "docs/api-reference.md is out of date — regenerate with 'make api-reference' and commit" >&2; \
		git --no-pager diff -- docs/api-reference.md | head -80 >&2; \
		exit 1; \
	fi
	@echo "docs/api-reference.md is up to date"

##@ Benchmarks (Phase S11)
#
# bench runs the unit benchmarks under internal/migration/analyze/.
# bench-integration runs the envtest-driven benchmarks under
# internal/controller/ (build tag `benchmark`). The latter needs the
# envtest K8s assets — `make envtest` first.

.PHONY: bench
bench: ## Run unit benchmarks (analyzer pack) — benchstat-friendly output
	$(GO) test -bench=. -benchmem -count=5 -run=^$$ ./internal/migration/analyze/...

.PHONY: bench-integration
bench-integration: envtest ## Run integration benchmarks (controller, envtest-backed)
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		$(GO) test -tags=benchmark -bench=. -benchmem -count=3 -run=^$$ -timeout=20m ./internal/controller/...

.PHONY: policy-test
policy-test: ## Run conftest unit tests on Rego policies
	@if ! command -v conftest >/dev/null 2>&1; then \
		echo "conftest not on PATH — install from https://github.com/open-policy-agent/conftest" >&2; \
		exit 1; \
	fi
	conftest verify --policy policy/conftest

.PHONY: lint-sql
lint-sql: ## Run squawk on SQL files (pass paths as ARGS=)
	hack/lint-sql.sh $${ARGS:-../../<gitops-repo>/keystone/migrations}

##@ SAST (Phase 12 — gosec + semgrep)
#
# Both tools run via Docker to avoid requiring local installs.
# Mirrors the keystone:gosec and keystone:semgrep CI jobs exactly.
# Digest placeholders match what is baked into the CI configuration — update both
# locations together when pinning.
#
# SAST image digest placeholders (fill in alongside the CI configuration pins):
#   securego/gosec:2.21.4     @sha256:__REPLACE_ME__
#   semgrep/semgrep:1.72.0    @sha256:__REPLACE_ME__

GOSEC_IMAGE   ?= docker.io/securego/gosec:2.21.4
SEMGREP_IMAGE ?= docker.io/semgrep/semgrep:1.72.0

.PHONY: sast-gosec
sast-gosec: ## Run gosec locally (HIGH severity fails); output: bin/gl-sast-gosec.json
	@mkdir -p $(BIN_DIR)
	docker run --rm \
		-v "$(abspath .):/code:ro" \
		-w /code \
		$(GOSEC_IMAGE) \
		-severity high \
		-fmt gitlab \
		-out /code/bin/gl-sast-gosec.json \
		./...
	@echo "gosec report written to $(BIN_DIR)/gl-sast-gosec.json"

.PHONY: sast-semgrep
sast-semgrep: ## Run semgrep locally (advisory, not gating); output: bin/semgrep.sarif
	@mkdir -p $(BIN_DIR)
	docker run --rm \
		-v "$(abspath ../..):/src:ro" \
		-w /src \
		-e SEMGREP_SEND_METRICS=off \
		$(SEMGREP_IMAGE) \
		semgrep ci \
		--config p/default \
		--config p/golang \
		--config p/security-audit \
		--sarif \
		--output services/keystone/bin/semgrep.sarif \
		services/keystone/ || true
	@echo "semgrep report written to $(BIN_DIR)/semgrep.sarif (advisory)"

.PHONY: sast
sast: sast-gosec sast-semgrep ## Run all SAST tools locally (gosec + semgrep)
	@echo "SAST complete — see $(BIN_DIR)/gl-sast-gosec.json and $(BIN_DIR)/semgrep.sarif"

##@ Supply-chain (Phase 9.3)

SYFT_VERSION        ?= v1.19.0
TRIVY_VERSION       ?= v0.58.2
COSIGN_VERSION      ?= v2.4.1
IMAGE               ?= ghcr.io/dogukanturhal/keystone-manager:$(VERSION)
SBOM_OUT            ?= $(BIN_DIR)/keystone-sbom.spdx.json

.PHONY: sbom
sbom: ## Generate SBOM (SPDX JSON) for the compiled binary + module graph
	@if ! command -v syft >/dev/null 2>&1; then \
		echo "syft not on PATH — install: curl -sSfL https://raw.githubusercontent.com/anchore/syft/main/install.sh | sh -s -- -b ./bin $(SYFT_VERSION)" >&2; \
		exit 1; \
	fi
	syft . --output spdx-json=$(SBOM_OUT)
	@echo "SBOM written to $(SBOM_OUT)"

.PHONY: vuln-scan
vuln-scan: ## Scan filesystem + Go modules for CVEs; fail on HIGH+CRITICAL with fixes
	@if ! command -v trivy >/dev/null 2>&1; then \
		echo "trivy not on PATH — install: curl -sfL https://raw.githubusercontent.com/aquasecurity/trivy/main/contrib/install.sh | sh -s -- -b ./bin $(TRIVY_VERSION)" >&2; \
		exit 1; \
	fi
	trivy fs --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 .
	@echo "CVE scan clean (no HIGH+CRITICAL with published fix)"

.PHONY: vuln-scan-image
vuln-scan-image: ## Scan the built manager image
	trivy image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 $(IMAGE)

.PHONY: sign
sign: ## Cosign-sign the manager image (keyless OIDC via GitLab CI identity)
	@if ! command -v cosign >/dev/null 2>&1; then \
		echo "cosign not on PATH — install from https://github.com/sigstore/cosign/releases" >&2; \
		exit 1; \
	fi
	cosign sign --yes $(IMAGE)

.PHONY: verify-signature
verify-signature: ## Verify the manager image signature
	cosign verify $(IMAGE) \
		--certificate-identity-regexp 'https://github.com/dogukanturhal/.*' \
		--certificate-oidc-issuer https://token.actions.githubusercontent.com

.PHONY: supply-chain
supply-chain: sbom vuln-scan ## Run the full supply-chain pipeline locally
	@echo "supply-chain: SBOM + vuln scan both clean"

##@ Git hooks (DCO enforcement)

.PHONY: install-hooks
install-hooks: ## Install DCO commit-msg hook into .git/hooks/
	@if [ ! -d .git ]; then \
		echo "install-hooks: .git directory not found — run from repository root" >&2; \
		exit 1; \
	fi
	@mkdir -p .git/hooks
	@ln -sf ../../services/keystone/hack/git-hooks/commit-msg .git/hooks/commit-msg
	@chmod +x services/keystone/hack/git-hooks/commit-msg
	@echo "DCO commit-msg hook installed — commits now require Signed-off-by trailer"

.PHONY: verify-hook
verify-hook: ## Verify the commit-msg hook is installed and executable
	@test -L .git/hooks/commit-msg || { echo "verify-hook: .git/hooks/commit-msg is not a symlink — run 'make install-hooks'" >&2; exit 1; }
	@test -x services/keystone/hack/git-hooks/commit-msg || { echo "verify-hook: hook script not executable" >&2; exit 1; }
	@echo "DCO commit-msg hook installed correctly"

##@ Tools (installed under bin/, pinned)

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Install controller-gen
$(CONTROLLER_GEN): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) $(GO) install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Install kustomize
$(KUSTOMIZE): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) $(GO) install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

.PHONY: envtest
envtest: $(ENVTEST) ## Install setup-envtest
$(ENVTEST): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) $(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Install golangci-lint
$(GOLANGCI_LINT): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) $(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: crd-ref-docs
crd-ref-docs: $(CRD_REF_DOCS) ## Install crd-ref-docs (elastic/crd-ref-docs) for API reference generation
$(CRD_REF_DOCS): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) $(GO) install github.com/elastic/crd-ref-docs@$(CRD_REF_DOCS_VERSION)

$(LOCALBIN):
	@mkdir -p $(LOCALBIN)

##@ Cleanup

.PHONY: clean
clean: ## Remove build artifacts (preserves pinned tools)
	rm -rf cover.out cover.html $(MANAGER_BIN)

.PHONY: clean-tools
clean-tools: ## Remove pinned tool binaries (forces re-download)
	rm -rf $(LOCALBIN)

##@ Docker

.PHONY: docker
docker: ## Build the manager image (run from repo root)
	@echo "Run from repo root: docker build -f services/keystone/Dockerfile -t keystone-manager:$(VERSION) ."

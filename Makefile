# Code-gen tools are pinned via go.mod `tool` directives (go 1.24+): one
# source of truth, dependabot bumps them, `go tool` resolves reproducibly.
CONTROLLER_GEN = go tool controller-gen
SETUP_ENVTEST  = go tool setup-envtest

# Tools we only ever shell out to (never import), so they stay out of go.mod
# and its dependency graph. Pinned here and ONLY here: the workflows invoke
# them through these targets, so a bump lands in CI and in `make verify` at
# the same time — a version skew between the two makes local green and CI red
# different statements.
HELM_DOCS  = go run github.com/norwoodj/helm-docs/cmd/helm-docs@v1.14.2
ACTIONLINT = go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.10

# Must match the version ci.yaml feeds golangci-lint-action, or `make lint`
# green and CI red stop being the same statement. Bump both together.
GOLANGCI_VERSION = v2.12.2
GOLANGCI = go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

IMG ?= ghcr.io/dklesev/karpenter-provider-ssh
TAG ?= dev
# Lazy (=), not immediate (:=): with := this shells out on EVERY make
# invocation, including `make help`. Only karpenter-crds reads it.
KARPENTER_VERSION = $(shell go list -m -f '{{.Version}}' sigs.k8s.io/karpenter)
CHART := charts/karpenter-provider-ssh

# A bare `make` should tell you what there is, not silently build.
.DEFAULT_GOAL := help

.PHONY: all
all: build

.PHONY: help
help: ## list targets
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: generate
generate: ## regenerate deepcopy funcs for the API types
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./pkg/apis/..."

.PHONY: manifests
manifests: ## regenerate our CRDs into config/crd + the chart
	$(CONTROLLER_GEN) crd paths="./pkg/apis/..." output:crd:artifacts:config=config/crd
	cp config/crd/*.yaml $(CHART)/crds/

# In `verify` (and therefore CI) because dependabot bumping
# sigs.k8s.io/karpenter otherwise leaves these vendored copies stale with
# nothing to notice: `make install` would then apply CRDs older than the core
# compiled into the binary.
.PHONY: karpenter-crds
karpenter-crds: ## copy karpenter core CRDs (NodePool/NodeClaim) from the pinned module
# The explicit download matters: nothing before this target compiles a package
# that imports karpenter, so on a cold module cache (CI cache miss after any
# go.sum change) the module directory would not exist and the cp would fail.
	go mod download sigs.k8s.io/karpenter
	mkdir -p config/karpenter
	cp $$(go env GOMODCACHE)/sigs.k8s.io/karpenter@$(KARPENTER_VERSION)/pkg/apis/crds/*.yaml config/karpenter/
	chmod 644 config/karpenter/*.yaml

.PHONY: build
build: generate ## build the controller binary into bin/
	go build -o bin/controller ./cmd/controller

.PHONY: run
run: build ## dev mode against current kubeconfig
	./bin/controller

.PHONY: test
test: generate ## unit tests (no cluster needed)
	go test ./...

# Native Go fuzzing of the parsers that take bytes off the wire or from the
# API server (SSHSIG, pinned host keys, probe output). Plain `go test` runs
# only the seed corpus; this drives the engine for a bounded time per target.
# Go accepts one -fuzz pattern per invocation, hence one line per target.
FUZZ_TIME ?= 30s
.PHONY: fuzz
fuzz: ## run each Fuzz* target for $(FUZZ_TIME) (default 30s)
	go test -run '^$$' -fuzz '^FuzzVerifySSHSIG$$' -fuzztime $(FUZZ_TIME) ./internal/sshexec/
	go test -run '^$$' -fuzz '^FuzzPinnedFingerprintFromKnownHost$$' -fuzztime $(FUZZ_TIME) ./internal/sshexec/
	go test -run '^$$' -fuzz '^FuzzParseProbeOutput$$' -fuzztime $(FUZZ_TIME) ./pkg/controllers/

ENVTEST_K8S_VERSION ?= 1.34.x
# -count=1 for the same reason as e2e below, if less acutely: this drives a real
# apiserver against the CRDs on disk. 14s is a cheap price for a gate that is
# certain to have run.
.PHONY: test-integration
test-integration: karpenter-crds ## envtest suite (real apiserver; downloads kubebuilder assets on first run)
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(PWD)/bin -p path)" \
		go test ./pkg/controllers/ -count=1 -run TestIntegration -v -timeout 5m

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: lint
lint: ## golangci-lint (same version and config as CI)
	$(GOLANGCI) run

.PHONY: lint-actions
lint-actions: ## actionlint over .github/workflows (same pin as CI)
	$(ACTIONLINT) -color

# The shim runs as root on every pool host and the signer runs in CI: load-
# bearing shell that gets the same bar as the Go. CI calls this target rather
# than its own inline shellcheck, so `make lint-shell` is the whole gate and
# the two cannot disagree on files or severity.
SHELL_SRC = shim/kpssh-shim hack/sign-profile.sh

.PHONY: lint-shell
lint-shell: ## shellcheck the shipped shell (same severity as CI)
	shellcheck -S warning $(SHELL_SRC)

# Docs are a shipped artifact here (there is a site), and a dead link is the
# one defect that renders green: the site builds, GitHub renders, only the
# reader finds out. No deps beyond python3 so it runs anywhere `make verify`
# does.
.PHONY: lint-docs
lint-docs: ## check every relative markdown link + #anchor resolves
	python3 hack/lint-docs.py

.PHONY: helm-lint
helm-lint: ## helm lint + template smoke: default values AND every optional object on
	helm lint $(CHART)
	helm template smoke $(CHART) -n kpssh-system >/dev/null
# networkPolicy, podDisruptionBudget and serviceMonitor all default to
# `enabled: false`, so the render above never reaches them: all three could be
# broken templates and this target would stay green until a user turned one on
# and got the traceback. Render the everything-on permutation too. replicas=2
# because the PDB refuses to render at its floor (see the template).
	helm template smoke $(CHART) -n kpssh-system \
		--set networkPolicy.enabled=true \
		--set podDisruptionBudget.enabled=true \
		--set serviceMonitor.enabled=true \
		--set replicas=2 >/dev/null
# The PDB guard itself: a chart that only fails when someone holds it wrong is
# worth nothing if the failing stops silently. This asserts the refusal.
	@if helm template smoke $(CHART) -n kpssh-system --set podDisruptionBudget.enabled=true >/dev/null 2>&1; then \
		echo "FAIL: chart rendered a PDB at minAvailable == replicas — that PDB blocks draining its own node"; exit 1; \
	fi
# Percentages coerce to 0 under `int`, sailing past the numeric guard while
# still deadlocking a drain (50% of 1 replica rounds up to the floor) — the
# chart must reject the string form, and this pins that it keeps doing so.
	@if helm template smoke $(CHART) -n kpssh-system --set podDisruptionBudget.enabled=true --set podDisruptionBudget.minAvailable=50% >/dev/null 2>&1; then \
		echo "FAIL: chart accepted a percentage minAvailable — bypasses the drain-safety guard via int coercion"; exit 1; \
	fi

.PHONY: helm-docs
helm-docs: ## regenerate chart README from values annotations
	$(HELM_DOCS) --chart-search-root charts

# The site generator is hash-pinned in hack/docs/requirements.txt and
# docs.yaml installs from the same file, so a local `make docs` and the
# published site cannot skew. Bump hack/docs/requirements.in, then
# `make docs-lock`. (Under hack/, not docs/: zensical publishes every
# non-Markdown file it finds in docs/.)
#
# Into a throwaway venv under bin/ (already gitignored), never a bare `pip
# install`: that is a hard error on any PEP 668 python — homebrew, Debian —
# and silently pollutes the user's environment on the rest.
DOCS_VENV = bin/docs-venv
DOCS_REQUIREMENTS = hack/docs/requirements.txt
ZENSICAL = $(DOCS_VENV)/bin/zensical

$(ZENSICAL): $(DOCS_REQUIREMENTS)
	python3 -m venv $(DOCS_VENV)
	$(DOCS_VENV)/bin/pip install --quiet --require-hashes -r $(DOCS_REQUIREMENTS)

# --universal: hashes for every platform wheel, so one lock installs on a
# macOS laptop and the ubuntu runner alike. --python-version must match the
# interpreter docs.yaml sets up.
.PHONY: docs-lock
docs-lock: ## regenerate hack/docs/requirements.txt (hashed) from requirements.in
	cd hack/docs && uv pip compile --universal --python-version 3.13 \
		--generate-hashes --no-header \
		--output-file requirements.txt requirements.in

.PHONY: docs
docs: $(ZENSICAL) ## build the docs site into site/ (same pin as CI)
	$(ZENSICAL) build --clean

.PHONY: docs-serve
docs-serve: $(ZENSICAL) ## live-preview the docs site on localhost
	$(ZENSICAL) serve

.PHONY: docker-build
docker-build: ## local single-arch image (no push)
	docker build --build-arg VERSION=$(TAG) -t $(IMG):$(TAG) .

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

.PHONY: docker-buildx
docker-buildx: ## multi-arch image (requires containerized buildx builder)
	docker buildx build --build-arg VERSION=$(TAG) --platform linux/amd64,linux/arm64 -t $(IMG):$(TAG) --push .

.PHONY: install
install: manifests karpenter-crds ## CRDs (ours + karpenter core) — greenfield clusters only
	@if kubectl get crd nodepools.karpenter.sh >/dev/null 2>&1; then \
		echo "karpenter.sh CRDs already exist (another karpenter owns them) — installing ONLY provider CRDs"; \
	else \
		kubectl apply -f config/karpenter; \
	fi
	kubectl apply -f config/crd

.PHONY: install-shared
install-shared: manifests ## ONLY provider CRDs — for clusters with an existing karpenter (coexistence)
	kubectl apply -f config/crd

# E2E_PARALLEL bounds Go's concurrent tests; E2E_MAX_CLUSTERS (read from the
# environment by the harness) is the hard cap on live kind clusters. One knob:
# MAX_CLUSTERS defaults to PARALLEL and is exported so `go test` inherits it.
# On a 2-vCPU CI runner, 2 is the practical ceiling before disk/CPU thrash.
E2E_PARALLEL ?= 3
export E2E_MAX_CLUSTERS ?= $(E2E_PARALLEL)

# -count=1 on both e2e targets: the result depends on docker state, the kind
# cluster, freshly built images and live SSH — none of which Go's test cache
# keys on. Without it `make e2e` can replay a previous PASS without starting a
# single container, which is the worst possible failure mode for a gate whose
# entire job is to touch real infrastructure.
.PHONY: e2e
e2e: ## full-loop e2e: parallel per-scenario kind clusters — raw (scale-up, consolidation, reboot, zombie) + verified exec (join, rogue signer, unsigned)
	go test -tags e2e -count=1 -timeout 40m -parallel $(E2E_PARALLEL) -v ./test/e2e/...

# The -timeout is a backstop, NOT a budget: the scenarios bound themselves with
# waitFor, which fails saying which wait expired. Go's timeout instead panics
# with a goroutine dump that says nothing about the cluster. So it must sit
# above the worst-case sum of one scenario's waits (reboot is the longest at
# ~31m of budget plus cluster setup) — otherwise the useful message is the one
# you never get.
.PHONY: e2e-one
e2e-one: ## run ONE e2e scenario in its own cluster (E2E_RUN=TestE2EScaleUp) — the CI matrix leg
	@test -n "$(E2E_RUN)" || { echo "E2E_RUN is required, e.g. make e2e-one E2E_RUN=TestE2EScaleUp"; exit 2; }
	go test -tags e2e -count=1 -timeout 40m -parallel 1 -v -run '^$(E2E_RUN)$$' ./test/e2e/...

.PHONY: license
license: ## apply Apache-2.0 headers to Go files
	go tool addlicense -f hack/license-header.txt ./cmd ./pkg ./internal ./test

.PHONY: verify-license
verify-license: ## fail on Go files missing the license header
	go tool addlicense -check -f hack/license-header.txt ./cmd ./pkg ./internal ./test

.PHONY: verify
verify: generate manifests karpenter-crds helm-docs verify-license lint-docs ## fail on any codegen/chart-README/license/docs-link drift (mirrors CI's codegen job)
	git diff --exit-code


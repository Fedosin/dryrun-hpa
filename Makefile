SHELL := /usr/bin/env bash -o pipefail
.SHELLFLAGS := -ec

MODULE := $(shell go list -m)
BIN := $(CURDIR)/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMG ?= localhost/dryrun-hpa:$(VERSION)
CONTAINER_TOOL ?= $(shell command -v podman >/dev/null 2>&1 && echo podman || echo docker)
KIND_CLUSTER ?= dryrun-hpa
CONTROLLER_GEN_VERSION ?= v0.23.0
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo $(BIN)/golangci-lint)

LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)
COMMANDS := dryrun-hpa-controller dryrun-hpactl fake-metrics-server loadgen

.PHONY: all
all: build

##@ Development

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  %-22s %s\n", $$1, $$2 } /^##@/ { printf "\n%s\n", substr($$0, 5) }' $(MAKEFILE_LIST)

.PHONY: generate
generate: $(BIN)/controller-gen ## Generate deepcopy, CRDs, RBAC, clientset, listers and informers.
	CONTROLLER_GEN=$(BIN)/controller-gen ./hack/update-codegen.sh
	go run ./hack/dashboards config/prometheus/dashboards
	./hack/update-chart.sh

.PHONY: sync-upstream
sync-upstream: ## Re-copy the upstream HPA controller and apply hack/upstream-patches.
	./hack/sync-upstream.sh

.PHONY: verify-upstream
verify-upstream: sync-upstream ## Fail if internal/upstream differs from upstream plus the patch series.
	git diff --exit-code -- internal/upstream

.PHONY: verify-generate
verify-generate: generate ## Fail if generated code is stale.
	git diff --exit-code -- api pkg/generated config/crd config/rbac config/prometheus/dashboards charts
	test -z "$$(git status --porcelain -- api pkg/generated config charts)"

.PHONY: fmt
fmt: ## Run gofmt.
	gofmt -s -w $$(git ls-files '*.go' | grep -v '^internal/upstream/')

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint.
	$(GOLANGCI_LINT) run ./...

.PHONY: test
test: ## Run unit tests, including the copied upstream HPA tests.
	go test -race -count=1 ./api/... ./internal/... ./cmd/...

.PHONY: test-upstream
test-upstream: ## Run only the copied upstream HPA tests.
	go test -count=1 ./internal/upstream/...

##@ Build

.PHONY: build
build: $(addprefix $(BIN)/,$(COMMANDS)) ## Build all binaries into bin/.

$(BIN)/%: FORCE
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/$*

.PHONY: FORCE
FORCE:

.PHONY: image
image: ## Build the container image.
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) -t $(IMG) .

##@ Deployment

.PHONY: kind-up
kind-up: ## Create a kind cluster with metrics-server.
	KIND_CLUSTER=$(KIND_CLUSTER) ./hack/kind-up.sh

.PHONY: kind-down
kind-down: ## Delete the kind cluster.
	kind delete cluster --name $(KIND_CLUSTER)

.PHONY: kind-load
kind-load: image ## Load the image into the kind cluster.
	./hack/kind-load.sh $(KIND_CLUSTER) $(IMG)

.PHONY: deploy
deploy: ## Deploy the controller with kustomize (config/default) and image IMG.
	./hack/deploy.sh config/default $(IMG)

.PHONY: deploy-e2e
deploy-e2e: ## Deploy the controller with the e2e settings (config/e2e).
	./hack/deploy.sh config/e2e $(IMG)

.PHONY: undeploy
undeploy: ## Remove the controller.
	kubectl delete -k config/default --ignore-not-found

##@ Testing

.PHONY: e2e
e2e: ## Run e2e tests against the current kube context (see test/e2e/README.md).
	go test -tags e2e -count=1 -timeout 60m -v ./test/e2e/...

.PHONY: perf-setup
perf-setup: ## Install kwok and fake-metrics-server and deploy config/perf with image IMG.
	IMG=$(IMG) ./test/perf/setup.sh

.PHONY: perf
perf: ## Run the Goal 1 performance matrix (see test/perf/README.md).
	./test/perf/run-matrix.sh

.PHONY: perf-teardown
perf-teardown: ## Undo perf-setup and reinstall metrics-server.
	./test/perf/teardown.sh

$(BIN)/controller-gen:
	GOBIN=$(BIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

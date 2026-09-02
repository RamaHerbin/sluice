# sluice developer targets.
#
# Written for GNU Make 3.81, the version shipped with macOS: no .ONESHELL,
# no ::=, no $(file ...). Every recipe line runs in its own shell, so multi
# step recipes are joined with ";" and "\" continuations.
#
# Run "make" or "make help" to list the targets.

GO           ?= go
BIN          := bin/sluice
IMAGE        ?= sluice:dev
KIND_CLUSTER ?= sluice
GOFMT_DIRS   := .
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO_LDFLAGS   := -s -w -X main.version=$(VERSION)

.PHONY: help fmt fmt-check vet lint test test-race check-boundary build clean \
        docker-build kind-up kind-load deploy undeploy demo kind-down ci

help: ## List the available targets (default)
	@echo "sluice: make <target>   (VERSION=$(VERSION))"; echo; \
	grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | \
	awk 'BEGIN {FS = ":.*## "} {printf "  %-16s %s\n", $$1, $$2}'

# --- code quality ----------------------------------------------------------

fmt: ## Format every Go source in place (gofmt -l -w .)
	gofmt -l -w $(GOFMT_DIRS)

fmt-check: ## Fail when gofmt would change any Go file
	@out="$$(gofmt -l $(GOFMT_DIRS))"; \
	if [ -n "$$out" ]; then \
	  echo "fmt-check: these files are not gofmt-clean (run make fmt):"; \
	  echo "$$out" | sed 's/^/  /'; \
	  exit 1; \
	fi; \
	echo "fmt-check: ok"

vet: ## Run go vet on every package
	$(GO) vet ./...

lint: ## Run golangci-lint (fails with an install hint when it is absent)
	@if command -v golangci-lint >/dev/null 2>&1; then \
	  golangci-lint version; \
	  golangci-lint run ./...; \
	else \
	  echo "lint: golangci-lint not found in PATH."; \
	  echo "lint: install v2.13.2, for example: brew install golangci-lint"; \
	  echo "lint: other installers: https://golangci-lint.run"; \
	  exit 1; \
	fi

test: ## Run the unit tests
	$(GO) test ./...

test-race: ## Run the unit tests with the race detector, no test cache
	$(GO) test -race -count=1 ./...

check-boundary: ## Enforce the k8s.io and prometheus import boundaries
	hack/check-boundary.sh

ci: fmt-check vet check-boundary test-race ## Everything the CI test job runs

# --- build -----------------------------------------------------------------

build: ## Build bin/sluice (CGO off, trimpath, stripped, version from git describe)
	@mkdir -p $(dir $(BIN))
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(GO_LDFLAGS)" -o $(BIN) ./cmd/sluice

clean: ## Remove build output
	rm -rf bin

docker-build: ## Build the container image $(IMAGE) (needs a running Docker daemon)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

# --- local cluster (kind) ----------------------------------------------------

kind-up: ## Create the kind cluster "$(KIND_CLUSTER)" if needed and wait for it
	KIND_CLUSTER=$(KIND_CLUSTER) hack/kind-up.sh

kind-load: ## Load $(IMAGE) into the kind cluster
	kind load docker-image $(IMAGE) --name $(KIND_CLUSTER)

deploy: ## Apply deploy/k8s/ (namespace, RBAC, Deployment, Service, IngressClass)
	kubectl apply -f deploy/k8s/

undeploy: ## Delete what deploy applied
	kubectl delete -f deploy/k8s/ --ignore-not-found

demo: ## Curl the demo Ingress through the kind port mapping and fetch metrics
	hack/demo.sh

kind-down: ## Delete the kind cluster "$(KIND_CLUSTER)"
	kind delete cluster --name $(KIND_CLUSTER)

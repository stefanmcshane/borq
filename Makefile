.PHONY: help test-unit test-integration-server test-integration-bootstrap promote-release release-dev dev fmt

GO_TEST_ENV := GOCACHE=/tmp/borq-go-build GOMODCACHE=/tmp/borq-go-mod
BORQ_INTEGRATION_SERVER_URL ?= http://localhost:7833
DEV_KIND_CLUSTER_NAME ?= dev-borq
DEV_KUBECONFIG_PATH ?= $(CURDIR)/tmp/dev/kubeconfig

help: ## Show available make targets.
	@awk '\
		BEGIN { \
			FS = ":.*## "; \
			printf "\nUsage:\n  make <target>\n\nTargets:\n"; \
		} \
		/^[a-zA-Z0-9_.-]+:.*## / { \
			printf "  %-28s %s\n", $$1, $$2; \
		} \
	' $(MAKEFILE_LIST)

test-unit: ## Run unit tests.
	$(GO_TEST_ENV) go test ./... -count=1

test-integration-server: ## Run server integration tests against a running borq server.
	BORQ_KUBECONFIG="$(DEV_KUBECONFIG_PATH)" BORQ_INTEGRATION_SERVER_URL="$(BORQ_INTEGRATION_SERVER_URL)" $(GO_TEST_ENV) go test -tags=integration ./api/server -run 'TestServerIntegration|TestReconcileIntegration' -v -count=1

test-integration-bootstrap: ## Run bootstrap integration tests on a fresh kind cluster. 
	# NOTE: Requires Docker daemon access (docker.sock) because kind runs in Docker.
	$(GO_TEST_ENV) go test -tags=integration ./business/managementbootstrap -run TestBootstrapIntegration -v -count=1

promote-release: ## Promote a tested RC digest to the release and latest tags without rebuilding.
	./zarf/scripts/promote_release.sh

release-dev: ## Build and publish a dev image tag to GHCR with GoReleaser (without updating latest).
	@tag="$$(date -u +%Y%m%d-%H%S).dev"; \
	builder="borq-release-$$(date -u +%Y%m%d%H%M%S)"; \
	previous_builder="$$(docker buildx ls 2>/dev/null | awk '/\*/ {print $$1; exit}')"; \
	trap 'status=$$?; \
		if [ -n "$$previous_builder" ]; then docker buildx use "$$previous_builder" >/dev/null 2>&1 || true; fi; \
		docker buildx rm -f "$$builder" >/dev/null 2>&1 || true; \
		git tag -d "$$tag" >/dev/null 2>&1 || true; \
		exit $$status' EXIT; \
	echo "releasing $$tag to ghcr.io/stefanmcshane/borq"; \
	docker buildx create --name "$$builder" --driver docker-container --use >/dev/null; \
	docker buildx inspect "$$builder" --bootstrap >/dev/null; \
	git tag "$$tag"; \
	goreleaser release --clean --config zarf/dev/.goreleaser.dev.yaml --skip=validate --auto-snapshot

dev: ## Ensure dev-borq exists, bootstrap borq, then run local borq with air via mirrord.
	@command -v mirrord >/dev/null || { echo "mirrord not found on PATH (install: brew install metalbear-co/mirrord/mirrord)"; exit 1; }
	@command -v air >/dev/null || { echo "air not found on PATH (install: go install github.com/air-verse/air@latest)"; exit 1; }
	@command -v kind >/dev/null || { echo "kind not found on PATH (install: https://kind.sigs.k8s.io/)"; exit 1; }
	@command -v kubectl >/dev/null || { echo "kubectl not found on PATH (install: https://kubernetes.io/docs/tasks/tools/)"; exit 1; }
	@mkdir -p "$(dir $(DEV_KUBECONFIG_PATH))"
	@KUBECONFIG="$(DEV_KUBECONFIG_PATH)" go run ./cmd/borq management bootstrap --cluster-name "$(DEV_KIND_CLUSTER_NAME)" --host-port 7833
	@kind get kubeconfig --name "$(DEV_KIND_CLUSTER_NAME)" > "$(DEV_KUBECONFIG_PATH)"
	@KUBECONFIG="$(DEV_KUBECONFIG_PATH)" kubectl -n borq-system wait --for=condition=Available deployment/borq --timeout=120s || { \
		echo "borq deployment did not become available in time"; \
		KUBECONFIG="$(DEV_KUBECONFIG_PATH)" kubectl -n borq-system get deploy,pods -o wide; \
		pod="$$(KUBECONFIG="$(DEV_KUBECONFIG_PATH)" kubectl -n borq-system get pods -l app.kubernetes.io/name=borq -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"; \
		if [ -n "$$pod" ]; then \
			KUBECONFIG="$(DEV_KUBECONFIG_PATH)" kubectl -n borq-system describe pod "$$pod"; \
		fi; \
		exit 1; \
	}
	@KUBECONFIG="$(DEV_KUBECONFIG_PATH)" BORQ_KUBECONFIG="$(DEV_KUBECONFIG_PATH)" mirrord exec -f zarf/dev/mirrord.json -- air -c zarf/dev/.air.toml

fmt:
	go fmt ./...
	go vet ./...

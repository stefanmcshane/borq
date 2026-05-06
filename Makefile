.PHONY: help test test-unit test-integration-server test-integration-local test-integration-bootstrap promote-release release-dev

GO_TEST_ENV := GOCACHE=/tmp/borq-go-build GOMODCACHE=/tmp/borq-go-mod
BORQ_INTEGRATION_SERVER_URL ?= http://localhost:7833

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
	BORQ_INTEGRATION_SERVER_URL="$(BORQ_INTEGRATION_SERVER_URL)" $(GO_TEST_ENV) go test -tags=integration ./api/server -run TestServerIntegration -v -count=1

# Legacy alias. Prefer test-integration-server.
test-integration-local:
	@$(MAKE) test-integration-server

test-integration-bootstrap: ## Run bootstrap integration tests on a fresh kind cluster.
	# NOTE: Requires Docker daemon access (docker.sock) because kind runs in Docker.
	$(GO_TEST_ENV) go test -tags=integration ./business/managementbootstrap -run TestBootstrapIntegration -v -count=1

promote-release: ## Promote a tested RC digest to the release and latest tags without rebuilding.
	./zarf/scripts/promote_release.sh

release-dev: ## Build and publish a dev image tag to GHCR with GoReleaser.
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
	goreleaser release --clean --config .goreleaser.yaml --skip=validate

.PHONY: help test test-integration release-dev

help: ## Show available make targets.
	@awk 'BEGIN {FS = ":.*## "}; /^[a-zA-Z0-9_.-]+:.*## / {printf "%-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

test: ## Run the full Go test suite.
	go test ./...

test-integration: ## Run the management bootstrap integration test.
	go test -tags=integration ./business/managementbootstrap -run TestExecuteIntegration -v -count=1

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

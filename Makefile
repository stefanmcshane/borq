.PHONY: help test test-integration release-dev

help: ## Show available make targets.
	@awk 'BEGIN {FS = ":.*## "}; /^[a-zA-Z0-9_.-]+:.*## / {printf "%-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

test: ## Run the full Go test suite.
	go test ./...

test-integration: ## Run the management bootstrap integration test.
	go test -tags=integration ./business/managementbootstrap -run TestExecuteIntegration -v -count=1

release-dev: ## Build and publish a dev image tag to GHCR with GoReleaser.
	@tag="$$(date -u +%Y%m%d-%H%S).dev"; \
	trap 'git tag -d "$$tag" >/dev/null 2>&1 || true' EXIT; \
	echo "releasing $$tag to ghcr.io/stefanmcshane/borq"; \
	git tag "$$tag"; \
	goreleaser release --clean --config .goreleaser.yaml

.PHONY: build-macos build-macos-bundle promote-release release-dev

MACOS_BUILD_DIR ?= $(CURDIR)/dist/macos

build-macos: ## Build the Apple Silicon macOS binary, archive, and checksum.
	@mkdir -p "$(MACOS_BUILD_DIR)/darwin_arm64"
	$(GO_TEST_ENV) CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o "$(MACOS_BUILD_DIR)/darwin_arm64/borq" ./cmd/borq
	tar -C "$(MACOS_BUILD_DIR)/darwin_arm64" -czf "$(MACOS_BUILD_DIR)/borq_darwin_arm64.tar.gz" borq
	cd "$(MACOS_BUILD_DIR)" && shasum -a 256 borq_darwin_arm64.tar.gz > SHA256SUMS

build-macos-bundle: build-macos ## Package Apple Silicon Borq with its offline server image and Borq-only setup instructions.
	@mkdir -p "$(MACOS_BUILD_DIR)/borq-local-dev-arm64" tmp/macos-server-bundle
	$(GO_TEST_ENV) CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o tmp/macos-server-bundle/borq ./cmd/borq
	@image_tag="borq-local:$$(shasum -a 256 tmp/macos-server-bundle/borq | cut -c1-16)"; \
	docker build --platform linux/arm64 --file business/cli/management/testdata/bootstrap.Dockerfile --tag "$$image_tag" tmp/macos-server-bundle && \
	docker save --output "$(MACOS_BUILD_DIR)/borq-local-dev-arm64/borq-server-linux-arm64.tar.tmp" "$$image_tag" && \
	mv "$(MACOS_BUILD_DIR)/borq-local-dev-arm64/borq-server-linux-arm64.tar.tmp" "$(MACOS_BUILD_DIR)/borq-local-dev-arm64/borq-server-linux-arm64.tar"
	cp "$(MACOS_BUILD_DIR)/darwin_arm64/borq" "$(MACOS_BUILD_DIR)/borq-local-dev-arm64/borq"
	cp business/cli/management/bundle/INSTALL.txt business/cli/management/bundle/apps.yaml "$(MACOS_BUILD_DIR)/borq-local-dev-arm64/"
	rm -f "$(MACOS_BUILD_DIR)/borq-local-dev-arm64/setup.sh"
	tar -C "$(MACOS_BUILD_DIR)" -czf "$(MACOS_BUILD_DIR)/borq-local-dev-arm64.tar.gz" borq-local-dev-arm64/borq borq-local-dev-arm64/borq-server-linux-arm64.tar borq-local-dev-arm64/INSTALL.txt borq-local-dev-arm64/apps.yaml
	cd "$(MACOS_BUILD_DIR)" && shasum -a 256 borq-local-dev-arm64.tar.gz > borq-local-dev-arm64.sha256

promote-release: ## Promote a tested RC digest to the release and latest tags without rebuilding.
	./zarf/scripts/promote_release.sh

release-dev: ## Build and publish a dev image tag to GHCR without updating latest.
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

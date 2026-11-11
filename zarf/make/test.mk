.PHONY: pre-commit test-slow test-dev test-unit test-pod-history test-ui test-apps test-managed-addons test-apps-e2e generate-api dev-cluster dev-certificates dev-secrets test-integration-server test-bootstrap test-platform-sdk test-platform-snapshots test-platform-vault test-integration-bootstrap test-custom-charts test-telemetry test-chart-defaults fmt

# The older application acceptance fixture stays separate from the full dev installation.
.PHONY: test-db-test test-db-test-postgres
test-db-test: ## Verify the shared database fixture, SQLite snapshot recovery and connection errors.
	$(GO_TEST_ENV) $(BORQ_GO) test ./api/dbtest ./cmd/db-test -count=1 $(ARGS)

test-db-test-postgres: ## Verify the same fixture against a real disposable Postgres database URL.
	@test -n "$$BORQ_ADDON_TEST_POSTGRES_URL" || (echo 'BORQ_ADDON_TEST_POSTGRES_URL is required' >&2; exit 1)
	$(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./api/dbtest -run TestPostgresWorkload -count=1 -timeout=2m $(ARGS)

BORQ_INTEGRATION_SERVER_URL ?= http://localhost:7833
BORQ_MANAGEMENT_KUBECONFIG ?= $(CURDIR)/tmp/management/kubeconfig
DEV_KUBECONFIG_PATH ?= $(CURDIR)/tmp/dev/kubeconfig
MANAGEMENT_KUBECONFIG_PATH ?= $(CURDIR)/tmp/management/kubeconfig
K3D ?= $(if $(wildcard $(CURDIR)/tmp/tools/k3d),$(CURDIR)/tmp/tools/k3d,$(if $(shell command -v k3d 2>/dev/null),k3d,$(CURDIR)/tmp/tools/k3d))
export K3D

pre-commit: ## Run formatting, unit tests, and slow cluster checks before committing.
	$(MAKE) fmt
	$(MAKE) test-unit
	$(MAKE) test-slow

test-slow: test-integration-server ## Run the slow server cluster suite; required before committing.

test-dev: test-dev-build ## Verify hot reload, restart persistence, full cleanup, and fresh setup on an isolated dev cluster.
	@$(GO_TEST_ENV) bash zarf/dev/test-session.sh

.PHONY: test-dev-build
test-dev-build: install-ui ## Verify build locking, failed UI publication, and Air recovery without a cluster.
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s zarf/dev -p 'test_build.py' -v

test-unit: build-ui check-ui test-ui-unit ## Run unit tests.
	$(GO_TEST_ENV) $(BORQ_GO) test ./... -count=1
	$(MAKE) test-pod-history
	$(MAKE) test-install-worker

.PHONY: test-install-worker
test-install-worker: ## Verify installer version selection and real shell download/install behavior.
	node --max-old-space-size=256 --test --test-concurrency=1 --test-timeout=15000 zarf/install-worker/test/*.test.mjs

test-pod-history: ## Verify per-release failed pod retention (requires Python 3).
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s business/server/platform/testdata/pod_history -p '*_test.py'

test-ui: build-ui check-ui test-ui-unit ## Verify the embedded Astro site and UI JSON API, optionally with ARGS.
	$(GO_TEST_ENV) $(BORQ_GO) test ./api/webui -count=1 $(ARGS)

.PHONY: test-ui-unit test-ui-browser
test-ui-unit: install-ui ## Verify local PostgreSQL configuration and naming helpers.
	npm test --prefix $(UI_DIR)

test-ui-browser: build install-ui ## Drive the embedded site in Chromium against a temporary standalone server.
	BORQ_UI_TEST_BINARY="$(CURDIR)/dist/borq" npm run test:browser --prefix $(UI_DIR)

test-apps: ## Run application manager tests, optionally with ARGS for focused checks.
	$(GO_TEST_ENV) $(BORQ_GO) test ./business/server/apps ./foundations/kubeapps ./foundations/sqlite ./api/appapi ./api/appmcp ./api/appserver ./api/cli -count=1 $(ARGS)

.PHONY: test-app-ingress-migration
test-app-ingress-migration: ## Verify Ingress retirement after real Gateway acceptance and certificate issuance on disposable K3s.
	$(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/apps -run '^TestApplicationIngressMigrationIntegration$$' -v -count=1 -timeout=8m $(ARGS)

.PHONY: test-oidc
test-oidc: ## Verify organization OIDC setup, secret storage, and browser sign-in tests.
	$(GO_TEST_ENV) $(BORQ_GO) test ./foundations/oidc ./business/server/apps ./api/appapi ./api/appserver ./api/webui -run Oidc -count=1 $(ARGS)

.PHONY: test-pocketid test-pocketid-integration test-pocketid-protocol-integration
test-pocketid: ## Verify managed Pocket ID configuration, provisioning protocol and UI boundaries.
	$(GO_TEST_ENV) $(BORQ_GO) test ./foundations/pocketid ./business/server/platform ./business/server/apps ./api/appapi ./api/appserver ./api/webui -run 'PocketId|OrganizationOidc' -count=1 $(ARGS)

test-pocketid-protocol-integration: ## Verify the Pocket ID API against its pinned disposable container.
	$(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./foundations/pocketid -run TestManagedPocketIdProtocol -count=1 -timeout=3m $(ARGS)

test-pocketid-integration: test-pocketid-protocol-integration ## Verify Pocket ID against its real pinned container and management Kubernetes.
	BORQ_KUBECONFIG="$(BORQ_MANAGEMENT_KUBECONFIG)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/platform -run TestPocketIdIntegration -count=1 -timeout=6m $(ARGS)

.PHONY: test-management-auth-integration
test-management-auth-integration: ## Verify managed Pocket ID YAML provisioning and OIDC persistence against management Kubernetes.
	BORQ_MANAGEMENT_KUBECONFIG="$(BORQ_MANAGEMENT_KUBECONFIG)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/apps -run TestManagementPocketIdIntegration -count=1 -timeout=8m $(ARGS)

test-managed-addons: ## Verify managed addon configuration, storage, runtime, and public surfaces without live clusters.
	$(GO_TEST_ENV) $(BORQ_GO) test ./foundations/objectstore ./foundations/openbao ./foundations/postgres ./business/server/sqliteworkload ./business/server/platform ./business/server/apps ./api/appapi ./api/appmcp ./api/cli ./api/webui -count=1 $(ARGS)

.PHONY: test-managed-postgres-e2e
test-managed-postgres-e2e: ## Create, seed, back up, restore, compare, and remove real managed Postgres addons.
	@mkdir -p tmp
	$(GO_TEST_ENV) $(BORQ_GO) build -o tmp/borq-managed-postgres-e2e ./cmd/borq
	BORQ_MANAGEMENT_KUBECONFIG="$(BORQ_MANAGEMENT_KUBECONFIG)" BORQ_MANAGED_ADDON_TEST_BINARY="$(CURDIR)/tmp/borq-managed-postgres-e2e" BORQ_MANAGED_ADDON_TEST_SERVER_URL="$(BORQ_INTEGRATION_SERVER_URL)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./api/appserver -run TestManagedPostgresIntegration -v -count=1 -timeout=35m $(ARGS)

test-apps-e2e: ## Deploy apps on local k3d, test conversions and CLI exit codes using temporary servers and SQLite state.
	@mkdir -p tmp
	$(GO_TEST_ENV) $(BORQ_GO) build -o tmp/borq-apps-e2e ./cmd/borq
	BORQ_KUBECONFIG="$(DEV_KUBECONFIG_PATH)" BORQ_APPLICATION_TEST_BINARY="$(CURDIR)/tmp/borq-apps-e2e" BORQ_INTEGRATION_SERVER_URL= $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./api/appserver ./api/cli -run 'TestApplicationIntegration|TestApplicationCliIntegration|TestPredeployIntegration' -v -count=1 $(ARGS)

generate-api: ## Regenerate the application API client and server from OpenAPI.
	$(GO_TEST_ENV) $(BORQ_GO) generate ./api/appapi

dev-cluster: ## Create or start the local k3d cluster and save its separate kubeconfig.
	bash zarf/dev/ensure-k3d.sh "$(K3D)"
	@mkdir -p "$(dir $(DEV_KUBECONFIG_PATH))"
	@borq_cluster="$$("$(K3D)" cluster list borq-dev --no-headers)"; \
	if [ -n "$$borq_cluster" ]; then \
		"$(K3D)" cluster start borq-dev; \
	else \
		"$(K3D)" cluster create --config zarf/dev/k3d.yaml; \
	fi
	@umask 077; $(K3D) kubeconfig get borq-dev > "$(DEV_KUBECONFIG_PATH)"
	kubectl --kubeconfig "$(DEV_KUBECONFIG_PATH)" wait --for=condition=Ready nodes --all --timeout=180s
	# The existing Postgres integration test also creates an External Secrets PushSecret.
	kubectl --kubeconfig "$(DEV_KUBECONFIG_PATH)" apply --server-side --field-manager=borq-local-tests -f zarf/crds/externalsecrets/v2.4.0.yaml
	kubectl --kubeconfig "$(DEV_KUBECONFIG_PATH)" wait --for=condition=Established crd/pushsecrets.external-secrets.io --timeout=60s
	$(MAKE) dev-certificates DEV_KUBECONFIG_PATH="$(DEV_KUBECONFIG_PATH)"
	$(MAKE) dev-secrets DEV_KUBECONFIG_PATH="$(DEV_KUBECONFIG_PATH)"

dev-certificates: ## Install pinned cert-manager in borq-system for local HTTPS deployment tests.
	bash zarf/dev/install-certificates.sh "$(DEV_KUBECONFIG_PATH)"

dev-secrets: ## Install External Secrets and an OpenBao dev server in borq-system for local deployment tests.
	bash zarf/dev/install-secrets.sh "$(DEV_KUBECONFIG_PATH)"

test-integration-server: ## Run server integration tests against a running borq server.
	BORQ_KUBECONFIG="$(DEV_KUBECONFIG_PATH)" BORQ_INTEGRATION_SERVER_URL="$(BORQ_INTEGRATION_SERVER_URL)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./api/server ./foundations/kubeapps -run 'TestServerIntegration|TestReconcileIntegration' -v -count=1
	# App fixtures own their API and SQLite state; target installation is covered by test-integration-bootstrap.
	BORQ_KUBECONFIG="$(DEV_KUBECONFIG_PATH)" BORQ_INTEGRATION_SERVER_URL= $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./api/appserver -run 'TestApplicationIntegration|TestPredeployIntegration' -v -count=1

test-bootstrap: ## Run management bootstrap unit tests, optionally with ARGS.
	$(GO_TEST_ENV) $(BORQ_GO) test ./business/cli/management -count=1 $(ARGS)

.PHONY: test-dns-protocol test-network
test-dns-protocol: ## Verify Cloudflare DNS provider operations.
	$(GO_TEST_ENV) $(BORQ_GO) test ./foundations/cloudflare -count=1 $(ARGS)

test-network: test-dns-protocol ## Verify bootstrap networking, DNS inventory, and certificate authorization.
	$(GO_TEST_ENV) $(BORQ_GO) test ./business/server/platform ./business/server/apps ./business/cli/management ./api/appserver ./api/cli -run 'Network|Dns01|DnsInventory' -count=1 $(ARGS)

.PHONY: test-network-dns-integration test-network-bootstrap-integration
test-network-native-integration: ## Verify native Cloudflare manifests and guarded retirement on isolated Kubernetes, without contacting providers.
	$(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/platform -run TestNetworkNativeIntegration -v -count=1 -timeout=5m $(ARGS)

test-network-bootstrap-integration: ## Verify private bootstrap credentials and rotation against isolated Kubernetes resources.
	BORQ_MANAGEMENT_KUBECONFIG="$(BORQ_MANAGEMENT_KUBECONFIG)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/cli/management -run TestNetworkBootstrapIntegration -count=1 -timeout=3m $(ARGS)

test-network-dns-integration: ## Verify management DNS inventory against Kubernetes without calling a DNS provider.
	BORQ_MANAGEMENT_KUBECONFIG="$(BORQ_MANAGEMENT_KUBECONFIG)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/apps -run TestNetworkDnsInventoryIntegration -count=1 -timeout=3m $(ARGS)

test-platform-sdk: ## Verify Helm SDK install, unchanged reconciliation, and explicit upgrades on local k3d.
	BORQ_KUBECONFIG="$(DEV_KUBECONFIG_PATH)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./foundations/helm -run TestReleaseLifecycleIntegration -v -count=1

.PHONY: test-platform-k3s
test-platform-k3s: ## Verify HTTPRoute, real certificates, TLS traffic, and controller isolation on disposable K3s.
	$(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/platform -run TestK3sGatewayIntegration -v -count=1 -timeout=10m $(ARGS)

test-platform-snapshots: ## Verify the default snapshot chart, controller reconciliation, ownership, and repeat setup on a cluster without snapshots.
	BORQ_KUBECONFIG="$(DEV_KUBECONFIG_PATH)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/platform -run TestSnapshotControllerIntegration -v -count=1 -timeout=6m $(ARGS)

test-custom-charts: ## Verify custom chart defaults, real Helm installation, upgrade, CRDs, persistence, and uninstall.
	BORQ_KUBECONFIG="$(DEV_KUBECONFIG_PATH)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./api/appserver -run TestApplicationIntegrationCustomCharts -v -count=1 $(ARGS)

test-telemetry: ## Verify selected collection against the local management cluster and stores.
	BORQ_MANAGEMENT_KUBECONFIG="$(CURDIR)/tmp/management/kubeconfig" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/platform -run TestSelectiveTelemetryIntegration -v -count=1 -timeout=5m $(ARGS)

test-chart-defaults: ## Verify matching pinned public HTTP and OCI chart defaults without Kubernetes access.
	$(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./foundations/helm -run TestPublicChartDefaultsIntegration -v -count=1 -timeout=2m $(ARGS)

.PHONY: test-management-bootstrap
test-management-bootstrap: ## Verify API bootstrap, separate key export, and management readiness without a cluster.
	$(GO_TEST_ENV) $(BORQ_GO) test ./business/cli/management ./business/server/platform ./business/server/apps ./api/appserver ./api/cli -count=1 $(ARGS)

.PHONY: test-management-bootstrap-e2e
test-management-bootstrap-e2e: ## Verify API bootstrap, separate key export and restart on an explicitly selected disposable kind cluster.
	@test -n "$(BOOTSTRAP_TEST_CLUSTER)" -a -n "$(BORQ_MINIMAL_BOOTSTRAP_KUBECONFIG)" || { echo 'Set BOOTSTRAP_TEST_CLUSTER and BORQ_MINIMAL_BOOTSTRAP_KUBECONFIG for a disposable kind cluster.'; exit 1; }
	@mkdir -p tmp/bootstrap-vault/image
	$(GO_TEST_ENV) CGO_ENABLED=0 $(BORQ_GO) build -o tmp/bootstrap-vault/image/borq ./cmd/borq
	docker build --file business/cli/management/testdata/bootstrap.Dockerfile --tag borq-bootstrap-vault:local tmp/bootstrap-vault/image
	kind load docker-image borq-bootstrap-vault:local --name "$(BOOTSTRAP_TEST_CLUSTER)"
	BORQ_MINIMAL_BOOTSTRAP_KUBECONFIG="$(BORQ_MINIMAL_BOOTSTRAP_KUBECONFIG)" BORQ_INTEGRATION_BOOTSTRAP_IMAGE=borq-bootstrap-vault:local $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/cli/management -run TestManagementBootstrapMinimalIntegration -v -count=1 -timeout=8m $(ARGS)

test-platform-vault: ## Verify persistent production OpenBao initialization, unsealing, and recovery on local k3d.
	BORQ_KUBECONFIG="$(DEV_KUBECONFIG_PATH)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/platform -run TestPersistentVaultIntegration -v -count=1 -timeout=6m

.PHONY: test-platform-s3
test-platform-s3: ## Verify credential generations through the real management secrets controller.
	BORQ_MANAGEMENT_KUBECONFIG="$(CURDIR)/tmp/management/kubeconfig" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/platform -run TestSharedS3ProjectionIntegration -v -count=1 -timeout=3m $(ARGS)

.PHONY: test-registry-auth
test-registry-auth: ## Verify space and management image pull credentials through real OpenBao and ESO.
	BORQ_MANAGEMENT_KUBECONFIG="$(CURDIR)/tmp/management/kubeconfig" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/apps -run TestRegistryProjectionIntegration -v -count=1 -timeout=4m $(ARGS)

test-integration-bootstrap: ## Prepare isolated dev Kubernetes, then bootstrap Borq, link targets, and test secrets and deployment.
	bash zarf/dev/ensure-k3d.sh "$(K3D)"
	@mkdir -p tmp/bootstrap/image
	$(GO_TEST_ENV) CGO_ENABLED=0 $(BORQ_GO) build -o tmp/bootstrap/borq ./cmd/borq
	$(GO_TEST_ENV) CGO_ENABLED=0 GOOS=linux $(BORQ_GO) build -o tmp/bootstrap/image/borq ./cmd/borq
	docker build --file business/cli/management/testdata/bootstrap.Dockerfile --tag borq-bootstrap-test:local tmp/bootstrap/image
	docker save --output tmp/bootstrap/borq-server-linux-$$(go env GOARCH).tar borq-bootstrap-test:local
	BORQ_INTEGRATION_K3D="$(shell command -v $(K3D))" BORQ_INTEGRATION_BOOTSTRAP_BINARY="$(CURDIR)/tmp/bootstrap/borq" BORQ_INTEGRATION_BOOTSTRAP_IMAGE=borq-bootstrap-test:local $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/cli/management -run TestBootstrapIntegration -v -count=1 -timeout=30m $(ARGS)

fmt: ## Format and vet Go code.
	$(GO_TEST_ENV) $(BORQ_GO) fmt ./...
	$(GO_TEST_ENV) $(BORQ_GO) vet ./...

.PHONY: test-offsite
test-offsite: ## Verify streaming archive safety and independent recovery configuration.
	$(GO_TEST_ENV) $(BORQ_GO) test ./business/offsite ./foundations/objectstore -count=1 $(ARGS)

.PHONY: test-rustfs-offsite
test-rustfs-offsite: ## Prove real RustFS pod-restart persistence and offline Restic export/rehydration in an isolated namespace.
	@mkdir -p tmp/offsite-tools
	$(GO_TEST_ENV) $(BORQ_GO) build -o tmp/borq-offsite ./cmd/borq
	@set -eu; borq_restic_container=$$(docker create restic/restic:0.18.0@sha256:4cf4a61ef9786f4de53e9de8c8f5c040f33830eb0a10bf3d614410ee2fcb6120); \
	trap 'docker rm "$$borq_restic_container" >/dev/null' EXIT; \
	docker cp "$$borq_restic_container:/usr/bin/restic" tmp/offsite-tools/restic
	PATH="$(CURDIR)/tmp/offsite-tools:$$PATH" BORQ_KUBECONFIG="$(DEV_KUBECONFIG_PATH)" BORQ_OFFSITE_TEST_BINARY="$(CURDIR)/tmp/borq-offsite" BORQ_OFFSITE_SEEDED_POSTGRES="$(OFFSITE_POSTGRES)" BORQ_OFFSITE_NATIVE="$(OFFSITE_NATIVE)" BORQ_OFFSITE_MANAGEMENT_ONLY="$(OFFSITE_MANAGEMENT_ONLY)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/platform -run TestRustfsOffsiteIntegration -v -count=1 -timeout=25m $(ARGS)

.PHONY: test-rustfs-offsite-postgres
test-rustfs-offsite-postgres: ## Recover real seeded CNPG data from offline rehydrated objects using existing management controllers.
	$(MAKE) test-rustfs-offsite DEV_KUBECONFIG_PATH="$(MANAGEMENT_KUBECONFIG_PATH)" OFFSITE_POSTGRES=true

.PHONY: test-management-backups
test-management-backups: ## Verify native capture, encrypted artifact recovery and independent local policy state.
	$(GO_TEST_ENV) $(BORQ_GO) test ./foundations/sqlite ./foundations/openbao ./business/managementbackup ./business/offsite ./business/server/apps ./business/server/platform ./api/cli -count=1 $(ARGS)

.PHONY: test-management-backups-e2e
test-management-backups-e2e: ## Prove independent native captures, age encryption, management-only offsite export and source-absent SQLite/Raft recovery.
	$(MAKE) test-rustfs-offsite DEV_KUBECONFIG_PATH="$(DEV_KUBECONFIG_PATH)" OFFSITE_NATIVE=true OFFSITE_MANAGEMENT_ONLY=true

.PHONY: test-management-backups-postgres-e2e
test-management-backups-postgres-e2e: ## Prove mixed native management and genuine PostgreSQL recovery from one offsite rehydration.
	$(MAKE) test-rustfs-offsite DEV_KUBECONFIG_PATH="$(MANAGEMENT_KUBECONFIG_PATH)" OFFSITE_NATIVE=true OFFSITE_POSTGRES=true

RELEASE_SMOKE_ARCH ?= $(shell go env GOARCH)
RELEASE_SMOKE_BUILD_FLAGS ?=
.PHONY: test-backup-release-image
test-backup-release-image: ## Build the actual release Dockerfile locally and execute bundled backup/recovery CLIs without publishing.
	@mkdir -p tmp/backup-release-image/linux/$(RELEASE_SMOKE_ARCH)
	$(GO_TEST_ENV) CGO_ENABLED=0 GOOS=linux GOARCH=$(RELEASE_SMOKE_ARCH) $(BORQ_GO) build -trimpath -o tmp/backup-release-image/linux/$(RELEASE_SMOKE_ARCH)/borq ./cmd/borq
	docker build --platform linux/$(RELEASE_SMOKE_ARCH) --file zarf/Dockerfile --tag borq-backup-smoke:local $(RELEASE_SMOKE_BUILD_FLAGS) tmp/backup-release-image
	docker run --rm --entrypoint /usr/local/bin/restic borq-backup-smoke:local version
	docker run --rm borq-backup-smoke:local management backups list --help
	docker run --rm borq-backup-smoke:local management backups restore --help
	docker run --rm borq-backup-smoke:local management backups local list --help
	docker run --rm borq-backup-smoke:local management backups local restore --help
	docker run --rm borq-backup-smoke:local management keys --help

.PHONY: test-sqlite-e2e
test-sqlite-e2e: ## Verify non-root SQLite, scoped RustFS backups, lost-volume restore and retained storage.
	docker build --file business/server/apps/testdata/sqlite/Dockerfile --tag borq-sqlite-test:local business/server/apps/testdata/sqlite
	docker pull rustfs/rustfs:1.0.1
	docker pull quay.io/openbao/openbao:2.7.1
	docker pull litestream/litestream:0.3.14
	docker pull python:3.13-slim-bookworm@sha256:5024f48ba9441d4b13a95d3945abc6365538e3a31109833367a1923523c6efed
	@set -eu; borq_sqlite_images=$$(mktemp); \
	trap 'rm -- "$$borq_sqlite_images"' EXIT; \
	docker save --platform linux/$(shell go env GOARCH) --output "$$borq_sqlite_images" borq-sqlite-test:local rustfs/rustfs:1.0.1 quay.io/openbao/openbao:2.7.1 litestream/litestream:0.3.14 python:3.13-slim-bookworm@sha256:5024f48ba9441d4b13a95d3945abc6365538e3a31109833367a1923523c6efed; \
	$(K3D) image import --cluster borq-dev "$$borq_sqlite_images"
	BORQ_KUBECONFIG="$(DEV_KUBECONFIG_PATH)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/apps -run TestSqliteIntegration -v -count=1 -timeout=12m $(ARGS)

.PHONY: test-platform-eso
test-platform-eso: ## Verify isolated management and application ESO on a disposable empty cluster.
	BORQ_KUBECONFIG="$(DEV_KUBECONFIG_PATH)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/platform -run TestConvergedExternalSecretsIntegration -v -count=1 -timeout=6m $(ARGS)

.PHONY: test-kubernetes
test-kubernetes: ## Verify Kubernetes client behavior and API warning logging.
	$(GO_TEST_ENV) $(BORQ_GO) test ./foundations/kubernetes -count=1 $(ARGS)

.PHONY: test-space-pocketid-integration
test-space-pocketid-integration: ## Verify Space Pocket ID SQLite persistence and a single writer during StatefulSet rollover.
	BORQ_KUBECONFIG="$(DEV_KUBECONFIG_PATH)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/platform -run TestSpacePocketIdIntegration -v -count=1 -timeout=6m $(ARGS)

.PHONY: test-rustfs-encryption
test-rustfs-encryption: ## Verify native RustFS SSE with disposable OpenBao Transit and fail-closed object reads and writes.
	$(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/platform -run '^TestRustfsBackupEncryption$$' -count=1 -v -timeout=6m $(ARGS)

.PHONY: test-sqlite-encryption-integration
test-sqlite-encryption-integration: ## Prove pinned Litestream encrypts snapshot/WAL objects and restores only with its original vaulted identity.
	$(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/sqliteworkload -run TestEncryptedReplicationIntegration -v -count=1 -timeout=4m $(ARGS)

.PHONY: test-manual-recovery
test-manual-recovery: ## Prove manual static-seal Vault, Borq SQLite and encrypted app recovery after deleting an isolated source kind cluster.
	@mkdir -p tmp
	$(GO_TEST_ENV) $(BORQ_GO) build -o tmp/borq-manual-recovery ./cmd/borq
	BORQ_MANUAL_RECOVERY_BINARY="$(CURDIR)/tmp/borq-manual-recovery" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./business/server/platform -run '^TestManualStaticSealRecovery$$' -v -count=1 -timeout=8m $(ARGS)

.PHONY: test-release-oidc
test-release-oidc: ## Verify saved callback guidance and public/private OIDC login using an explicit CLI/server release binary.
	@test -n "$(BORQ_RELEASE_BINARY)" || (echo 'BORQ_RELEASE_BINARY is required' >&2; exit 1)
	BORQ_RELEASE_BINARY="$(BORQ_RELEASE_BINARY)" $(GO_TEST_ENV) $(BORQ_GO) test -tags=integration ./api/appserver -run '^TestReleaseOidc$$' -v -count=1 -timeout=2m $(ARGS)

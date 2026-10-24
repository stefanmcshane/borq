.PHONY: dev dev-wait logs kubeconfig k9s dev-stop dev-kill _dev-bootstrap _dev-configure

DEV_CLUSTER ?= management
DEV_ORGANIZATION ?= dev
DEV_SPACE ?= default
DEV_PROFILE ?= development
DEV_DIR ?= $(CURDIR)/tmp/management
DEV_DETACH ?= 0
DEV_HTTP_PORT ?= 8080
DEV_HTTPS_PORT ?= 18443
DEV_API_PORT ?= 7833
DEV_API_URL := http://localhost:$(DEV_API_PORT)
DEV_BORQ := $(DEV_DIR)/borq
DEV_KUBECONFIG := $(DEV_DIR)/kubeconfig

export DEV_CLUSTER DEV_ORGANIZATION DEV_SPACE DEV_PROFILE DEV_DIR DEV_DETACH DEV_HTTP_PORT DEV_HTTPS_PORT DEV_API_PORT DEV_API_URL DEV_BORQ DEV_KUBECONFIG

dev: ## Bootstrap one management/target cluster and run Air + Mirrord; DEV_DETACH=1 runs in background.
	@$(GO_TEST_ENV) bash zarf/dev/session.sh start

dev-wait: ## Wait for development bootstrap, hot reload, and target components to become ready.
	@bash zarf/dev/session.sh wait

logs: ## Follow local Borq output and Borq server pod logs; Ctrl-C only detaches the log view.
	@bash zarf/dev/session.sh logs

kubeconfig: ## Print the development kubeconfig path for use by other commands.
	@test -f "$(DEV_KUBECONFIG)" || { echo 'Development kubeconfig is missing; run make dev first.' >&2; exit 1; }
	@case "$(DEV_KUBECONFIG)" in \
		/*) printf '%s\n' "$(DEV_KUBECONFIG)" ;; \
		*) printf '%s\n' "$(CURDIR)/$(DEV_KUBECONFIG)" ;; \
	esac

k9s: ## Open k9s on the development cluster without changing kubectl's current context.
	@kubeconfig="$$($(MAKE) --no-print-directory -s kubeconfig)" && KUBECONFIG="$$kubeconfig" k9s

dev-stop: ## Stop local hot reload while retaining the cluster and development data.
	@bash zarf/dev/session.sh stop

dev-kill: ## Stop hot reload and delete the development cluster, storage, and local runtime data.
	@bash zarf/dev/session.sh kill

_dev-bootstrap:
	bash zarf/dev/ensure-k3d.sh "$(K3D)"
	@mkdir -p "$(DEV_DIR)/image"
	$(GO_TEST_ENV) go build -o "$(DEV_BORQ)" ./cmd/borq
	$(GO_TEST_ENV) CGO_ENABLED=0 GOOS=linux go build -o "$(DEV_DIR)/image/borq" ./cmd/borq
	@image="borq-dev:$$(shasum -a 256 "$(DEV_DIR)/image/borq" | cut -c1-16)"; \
	docker build --file business/cli/management/testdata/bootstrap.Dockerfile --tag "$$image" "$(DEV_DIR)/image" && \
	docker save --output "$(DEV_DIR)/borq-server-linux-$$(go env GOARCH).tar" "$$image" && \
	printf '%s\n' "$$image" > "$(DEV_DIR)/image.ref"
	bash zarf/dev/create-cluster.sh "$(K3D)" "$(DEV_CLUSTER)" "$(DEV_KUBECONFIG)" "$(DEV_HTTP_PORT)" "$(DEV_HTTPS_PORT)" "$(DEV_API_PORT)"
	"$(DEV_BORQ)" management bootstrap "$(DEV_CLUSTER)" --kubeconfig "$(DEV_KUBECONFIG)" --development \
		--api-url "$(DEV_API_URL)" --server-image "$$(cat "$(DEV_DIR)/image.ref")" \
		--server-image-archive "$(DEV_DIR)/borq-server-linux-$$(go env GOARCH).tar"

# These requests run only once the local server is ready and Mirrord is stealing traffic.
# The Air server keeps the authoritative dev database under DEV_DIR/data.
_dev-configure:
	"$(DEV_BORQ)" org create "$(DEV_ORGANIZATION)" --api-url "$(DEV_API_URL)" >/dev/null
	@set -e; linked_name="$(DEV_CLUSTER)"; \
		if test -s "$(DEV_DIR)/target-cluster"; then linked_name="$$(cat "$(DEV_DIR)/target-cluster")"; fi; \
		if ! curl --fail --silent --show-error "$(DEV_API_URL)/api/v1/organizations/$(DEV_ORGANIZATION)/clusters/$$linked_name/components" >/dev/null; then \
			node="$$(kubectl --kubeconfig "$(DEV_KUBECONFIG)" get nodes -l node-role.kubernetes.io/control-plane -o 'jsonpath={.items[0].status.addresses[?(@.type=="InternalIP")].address}')"; \
			test -n "$$node"; \
			"$(DEV_BORQ)" cluster link "$(DEV_CLUSTER)" --kubeconfig "$(DEV_KUBECONFIG)" \
				--api-server "https://$$node:6443" --organization "$(DEV_ORGANIZATION)" --api-url "$(DEV_API_URL)" \
				--reconcile=false --diff=false --output yaml > "$(DEV_DIR)/target-link.yaml"; \
			linked_name="$$(sed -n 's/^name: //p' "$(DEV_DIR)/target-link.yaml")"; \
			test -n "$$linked_name"; \
		fi; \
		printf '%s\n' "$$linked_name" > "$(DEV_DIR)/target-cluster"
	"$(DEV_BORQ)" cluster reconcile "$$(cat "$(DEV_DIR)/target-cluster")" --organization "$(DEV_ORGANIZATION)" --api-url "$(DEV_API_URL)" >/dev/null
	"$(DEV_BORQ)" space create "$(DEV_SPACE)" --cluster "$$(cat "$(DEV_DIR)/target-cluster")" --organization "$(DEV_ORGANIZATION)" --api-url "$(DEV_API_URL)" >/dev/null
	"$(DEV_BORQ)" config set --profile "$(DEV_PROFILE)" --api-url "$(DEV_API_URL)" \
		--organization "$(DEV_ORGANIZATION)" --cluster "$$(cat "$(DEV_DIR)/target-cluster")" --space "$(DEV_SPACE)" >/dev/null
	"$(DEV_BORQ)" config use "$(DEV_PROFILE)" >/dev/null

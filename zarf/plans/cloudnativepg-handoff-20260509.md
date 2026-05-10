# CloudNativePG Reconciliation Handoff

## Goal

Implement two separate reconciler components:

- `cloudnativepg-operator`: installs CloudNativePG operator version `1.29.0` into `cnpg-system`.
- `borq-postgres`: creates a small CNPG PostgreSQL cluster named `borq-postgres` in `borq-system`.

The operator must be reconciled first. The postgres database must be reconciled in a separate run.

Default `borq-postgres` cluster settings requested by user:

- Cluster name: `borq-postgres`
- Namespace: `borq-system`
- Storage: `5Gi`
- Database: `borq`
- Owner: `borq`
- Resource requests: `cpu: 100m`, `memory: 100Mi`

The user explicitly requested importing CNPG types directly:

```go
apiv1 "github.com/cloudnative-pg/cloudnative-ppg/api/v1"
```

Correct path used in implementation:

```go
apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
```

## Implemented So Far

### Dependencies

`go.mod` / `go.sum` were updated with:

- `github.com/cloudnative-pg/cloudnative-pg v1.29.0`
- Kubernetes modules upgraded to `v0.35.3`
- `sigs.k8s.io/controller-runtime v0.23.3`
- CNPG transitive dependencies/checksums added via:

```sh
go get github.com/cloudnative-pg/cloudnative-pg@v1.29.0 sigs.k8s.io/controller-runtime@v0.23.3 k8s.io/api@v0.35.3 k8s.io/apimachinery@v0.35.3 k8s.io/client-go@v0.35.3 k8s.io/apiextensions-apiserver@v0.35.3
go get github.com/cloudnative-pg/cloudnative-pg/api/v1@v1.29.0 github.com/cloudnative-pg/cloudnative-pg/pkg/utils@v1.29.0 github.com/cloudnative-pg/cloudnative-pg/pkg/postgres@v1.29.0
```

### Kubernetes Foundation

Edited `foundations/kubernetes/kubernetes.go`:

- Added `NewScheme()` that registers:
  - core Kubernetes scheme
  - admissionregistration v1
  - apiextensions v1
  - `apiv1.AddToScheme(scheme)`
- Added a controller-runtime client to `Connection`.
- Added `Connection.Objects()` returning a generic `ObjectManager`.
- Added `ObjectManager.Get`, `ObjectManager.Apply`, and `ObjectManager.Delete` for typed Kubernetes objects that are not represented by existing borq-local structs.
- Added `NewConnectionForTestWithObjects` for tests.

### Reconciler Components

Added `business/server/reconciler/cloudnativepg.go`.

Implemented:

- `CloudNativePGOperatorHandler`
- `BorqPostgresHandler`
- `CloudNativePGOperatorConfig`
- `BorqPostgresConfig`

Operator behavior:

- Uses code-owned defaults for namespace, service account, RBAC, config map, service, deployment, webhooks.
- Does not embed/parse the upstream full operator manifest at runtime for all objects.
- Important compromise: CNPG CRDs are loaded from CNPG module release manifest (`releases.OperatorManifests.ReadFile("cnpg-1.29.0.yaml")`) and converted into typed `apiextensionsv1.CustomResourceDefinition`. This avoids hand-copying huge generated CRD schemas while still keeping the operator defaults/components in Go.
- Current discovery fetches only exact desired objects, avoiding broad list/delete.
- Adds pseudo-resource `CloudNativePGOperatorReady` to wait for:
  - `deployment/cnpg-controller-manager` available
  - mutating and validating webhook configurations to have non-empty `clientConfig.caBundle`

Postgres behavior:

- Builds a typed `*apiv1.Cluster`.
- Defaults match user request.
- Supports config overrides for name, namespace, instances, storage size, database, owner, CPU request, memory request, and storage class.

### Server Wiring

Edited `api/server/server.go`:

- Added handlers:
  - `cloudnativepg-operator`
  - `borq-postgres`
- Added `moveNameToFront` so reconcile-all runs `cloudnativepg-operator` before alphabetically earlier `borq-postgres`.

### Tests Added

Edited/added:

- `business/server/reconciler/cloudnativepg_test.go`
- `foundations/kubernetes/kubernetes_test.go`
- `api/server/server_test.go`
- `api/server/integration_test.go`

Coverage added:

- Operator default config.
- Operator expected install resource set, including 10 CRDs.
- Operator readiness apply order after deployment.
- `borq-postgres` default config.
- typed `apiv1.Cluster` default and override behavior.
- `NewScheme` registers CNPG Cluster type.
- `ObjectManager` can apply/get/delete a typed CNPG Cluster with controller-runtime fake client.
- reconcile-all prioritizes `cloudnativepg-operator`.
- New integration test `TestReconcileIntegrationCloudNativePG`, using a local test server rather than external `localhost:7833`, then:
  - reconciles `cloudnativepg-operator`
  - waits for operator deployment
  - reconciles `borq-postgres` with test name `borq-postgres-integration`
  - waits for cluster `Ready=True` or `PhaseHealthy`

## Test Status

Passed:

```sh
make fmt
make test-unit
```

Specific CNPG integration initially passed once before stricter webhook readiness was added:

```sh
BORQ_KUBECONFIG=/home/dev/borq/tmp/dev/kubeconfig GOCACHE=/tmp/borq-go-build GOMODCACHE=/tmp/borq-go-mod go test -tags=integration ./api/server -run TestReconcileIntegrationCloudNativePG -v -count=1
```

That run passed in about 53s.

Then full required target was rerun:

```sh
make test-integration-server
```

It failed at `TestReconcileIntegrationCloudNativePG` with:

```text
reconcile failed: reconcile CloudNativePGCluster/borq-postgres-integration: create object "borq-postgres-integration": Internal error occurred: failed calling webhook "mcluster.cnpg.io": failed to call webhook: Post "https://cnpg-webhook-service.cnpg-system.svc:443/mutate-postgresql-cnpg-io-v1-cluster?timeout=10s": tls: failed to verify certificate: x509: certificate signed by unknown authority
```

Fix attempted:

- Updated operator readiness to wait for webhook CA bundles before returning success from `cloudnativepg-operator`.

After this fix:

```sh
make fmt
```

passed.

Then the specific CNPG integration test was started again:

```sh
BORQ_KUBECONFIG=/home/dev/borq/tmp/dev/kubeconfig GOCACHE=/tmp/borq-go-build GOMODCACHE=/tmp/borq-go-mod go test -tags=integration ./api/server -run TestReconcileIntegrationCloudNativePG -v -count=1
```

The user interrupted the turn while this command was still running/waiting. It may still be running in the background from session id `98211`, but the tool call was aborted by the user. No final result was captured after the readiness change.

## Known Issues / Likely Next Debugging

1. Verify whether the stricter webhook readiness wait completes.

Run:

```sh
BORQ_KUBECONFIG=/home/dev/borq/tmp/dev/kubeconfig GOCACHE=/tmp/borq-go-build GOMODCACHE=/tmp/borq-go-mod go test -tags=integration ./api/server -run TestReconcileIntegrationCloudNativePG -v -count=1
```

2. If it times out waiting for webhook CA bundles, inspect:

```sh
KUBECONFIG=/home/dev/borq/tmp/dev/kubeconfig kubectl get mutatingwebhookconfiguration cnpg-mutating-webhook-configuration -o yaml
KUBECONFIG=/home/dev/borq/tmp/dev/kubeconfig kubectl get validatingwebhookconfiguration cnpg-validating-webhook-configuration -o yaml
KUBECONFIG=/home/dev/borq/tmp/dev/kubeconfig kubectl -n cnpg-system logs deploy/cnpg-controller-manager
```

3. Possible root cause: our code-owned webhook configurations may omit annotations used by CNPG/operator/cert management to patch CA bundles, or the controller expects to own/patch them differently. The original release manifest should be compared for metadata/annotations on webhook configurations.

4. Operator deployment config map `cnpg-default-monitoring` is currently created with empty data. The release manifest has large default monitoring query data. This may not block startup, but if operator logs complain about monitoring config, either:
   - populate minimal/real monitoring config data from CNPG defaults, or
   - set the env var differently if CNPG supports empty/default behavior.

5. The object manager currently uses get/create/update, not server-side apply. That is consistent with existing borq managers but may conflict with CRD/webhook fields mutated by the operator. If update keeps removing CA bundles or operator-managed fields, switch `ObjectManager.Apply` to server-side apply or preserve live webhook `CABundle` values before update.

6. The readiness check itself fetches live webhook configs using `ObjectManager.Get`; this path should work because admissionregistration types are registered in `NewScheme()`.

## Files Touched

Expected changed files:

- `go.mod`
- `go.sum`
- `api/server/server.go`
- `api/server/server_test.go`
- `api/server/integration_test.go`
- `business/server/reconciler/cloudnativepg.go`
- `business/server/reconciler/cloudnativepg_test.go`
- `foundations/kubernetes/kubernetes.go`
- `foundations/kubernetes/kubernetes_test.go`
- `zarf/plans/cloudnativepg-handoff-20260509.md`

## Required Before Final Completion

Per `AGENTS.md`, because Go files changed, these must pass outside the sandbox before ending implementation:

```sh
make test-unit
make test-integration-server
make fmt
```

Current status:

- `make fmt`: passed after latest code change.
- `make test-unit`: passed before latest readiness tweak. Should rerun.
- `make test-integration-server`: not yet passing after latest readiness tweak; rerun and debug as above.


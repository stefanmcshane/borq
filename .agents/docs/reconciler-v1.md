# Reconciler Pattern — Design v1

## Core Interface

The reconciler follows the O'Reilly infrastructure-as-code pattern. Every
managed component (vault, keycloak, prometheus, etc.) should implement this
interface:

```go
type Reconciler[T any] interface {
    GetActual(ctx context.Context) (T, error)
    GetExpected(ctx context.Context) (T, error)
    Reconcile(ctx context.Context, actual, expected T, dryRun bool) (T, ReconcileResult, error)
    Destroy(ctx context.Context, actual T) (T, error)
}
```

The interface is declared but the codebase currently uses a single concrete
`ResourceSetReconciler` that operates on `Infrastructure` (a set of typed
resources). Per-component logic (vault, etc.) supplies resource manifests and
apply order; the reconciler handles the diff/apply/destroy loop.

## ResourceSetReconciler — Concrete Implementation

```go
type ResourceSetReconciler struct {
    conn kubernetes.Connection
    log  logger.Logger
}
```

Returned by value — the struct is lightweight and methods use value receivers:

```go
func NewResourceSetReconciler(conn kubernetes.Connection, log logger.Logger) ResourceSetReconciler
```

## GetActual — Label Discovery

`GetActual` uses **label-based `List` queries** — NOT name lookups. This
discovers everything bearing the component's labels, including resources
added or removed manually outside the reconciler.

```go
func (r ResourceSetReconciler) GetActual(ctx context.Context, selector string, namespaces []string) (Infrastructure, error)
```

The `selector` is a Kubernetes label selector (e.g.
`app.kubernetes.io/managed-by=borq,app.kubernetes.io/component=vault`).
`namespaces` restricts the search to specific namespaces for namespace-scoped
resources. Cluster-scoped resources (ClusterRole, ClusterRoleBinding) are
always queried without namespace filtering.

Internally it dispatches by kind:
- **Namespace-scoped:** Services, ConfigMaps, ServiceAccounts,
  PersistentVolumeClaims, StatefulSets — listed in each given namespace.
- **Cluster-scoped:** ClusterRoles, ClusterRoleBindings — listed once.

## GetExpected — Static Resource Construction

```go
func (r ResourceSetReconciler) GetExpected(resources []ResourceState, order []Kind) Infrastructure
```

Takes a pre-built list of desired resources and their apply order. It does
not read config itself — that is the component's responsibility. For vault:

```go
expected := managementreconcile.VaultResourceSet(config)
```

`VaultResourceSet` (in `vault.go`) constructs all vault manifests (Service,
ServiceAccount, ClusterRole, ClusterRoleBinding, ConfigMap, PersistentVolumeClaim,
StatefulSet) tagged with component labels so `GetActual` can discover them.

## Reconcile — Three-Way Merge

```go
func (r ResourceSetReconciler) Reconcile(
    ctx context.Context,
    actual, expected Infrastructure,
    dryRun bool,
) (Infrastructure, ReconcileResult, error)
```

Processes resources in `ApplyOrder`:

1. **Build a lookup** of actual resources by (Kind, Name)
2. **For each expected resource** (in apply order):
   - Not in actual → `Apply` (create)
   - In actual, differs → `Apply` (update, full replacement)
   - In actual, matches → no-op
3. **For each actual resource** not in expected → `Delete` (prune orphan)
4. **Return expected as new actual** (guarantee: infra now matches intent)

When `dryRun` is true, diffs are computed and returned but no mutations
are performed against the cluster.

## Apply Order & Destroy

Apply order is component-specific and defined as a package-level variable in
the component's source. For vault:

```go
var VaultApplyOrder = []Kind{
    KindServiceAccount,
    KindClusterRole,
    KindClusterRoleBinding,
    KindConfigMap,
    KindService,
    KindPersistentVolumeClaim,
    KindStatefulSet,
}
```

Resources are applied in this order and destroyed in reverse.

```go
func (r ResourceSetReconciler) Destroy(ctx context.Context, infra Infrastructure) (Infrastructure, error)
```

## Diff Computation

Diffs are computed inline during reconcile by `computePatch` (in `diff.go`).
The comparison serializes both actual and desired to `map[string]any` and
recursively compares. The result is a list of `DiffPatch` operations:

```go
type DiffPatch struct {
    Op    string      `json:"op"`
    Path  string      `json:"path"`
    Value interface{} `json:"value,omitempty"`
}
```

The API endpoint at `POST /reconcile` returns the full `ReconcileResult` as
JSON, including all resource diffs.

## Adding a New Component

1. Define the component's Kubernetes resource manifests as typed values
   (ServiceAccount, ClusterRole, etc.)
2. Define the apply order as a `[]Kind` variable
3. Write a function that returns `[]ResourceState` for the component
4. Call `NewResourceSetReconciler` + `GetActual` + `Reconcile` in the handler

No new interfaces, factories, or struct types are required. The
`ResourceSetReconciler` handles all resource types generically through its
kind-dispatch switch in `apply` and `destroyResource`.

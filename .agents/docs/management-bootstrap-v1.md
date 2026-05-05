# Management Bootstrap V1

## What Was Implemented

The current CLI entrypoint is:

```text
borq management bootstrap
```

This command currently bootstraps a **local management cluster** using kind.

Implemented behavior:

- Creates or reuses a kind cluster named `borq-management` by default.
- Uses a borq-owned default kind config built in Go, not a handwritten default YAML file.
- Configures host-to-cluster port mapping so the management liveness app is reachable on `localhost:7833`.
- Applies borq-owned Kubernetes resources into `borq-system`.
- Deploys the `traefik/whoami` container as the first management-cluster liveness workload.
- Exposes that workload with a `NodePort` Service mapped through kind.
- Waits for the Deployment to become available.
- Probes `http://localhost:7833` and only reports success after the host can reach the workload.
- Supports safe re-runs against the same cluster.

## Files Added Or Changed

- `cmd/borq/main.go`
  - Starts the CLI.
- `api/cli/root.go`
  - Defines the root command and registers the management command group.
- `api/cli/management_command.go`
  - Defines `borq management bootstrap` and its flags.
- `business/managementbootstrap/managementbootstrap.go`
  - Owns the management bootstrap workflow: kind orchestration, kubeconfig lookup, apply, readiness, probe.
- `business/managementbootstrap/managementbootstrap_test.go`
  - Unit tests for request defaults, validation, kind config rendering, and cluster detection helpers.
- `business/managementbootstrap/integration_test.go`
  - Live integration test using a real kind cluster and Docker.
- `foundations/kubernetes/kubernetes.go`
  - Borq-owned Kubernetes apply helpers using typed apply configs and server-side apply.
- `README.md`
  - Updated to match the current command shape and first-slice behavior.

## Why It Was Built This Way

### 1. Local-first before server-first

Borq’s README describes a long-term server-plus-CLI model, but the repo was effectively empty.
Starting with a local command proves the workflow first without freezing the wrong runtime architecture too early.

This keeps the first slice small while still leaving room to move the same workflow behind a borq server later.

### 2. Management bootstrap, not generic bootstrap

The package and command shape are intentionally management-specific.
This avoids a vague generic bootstrap package that would need to be split once workload-cluster bootstrap or other management operations arrive.

### 3. Typed defaults instead of a static default YAML

The default kind config is built from Go types and rendered to YAML only when kind needs a file.
That keeps borq’s runtime behavior and its defaults in one source of truth, which is easier to evolve than maintaining both code and a checked-in default YAML file.

### 4. Borq owns only borq resources

This slice does not install a full ingress controller or other management stack components yet.
It owns only:

- `borq-system`
- the whoami Deployment
- the whoami Service

That keeps the ownership boundary clear and the diff small.

### 5. Server-side apply for Kubernetes resources

Borq uses Kubernetes server-side apply for the resources it owns.
That gives borq an explicit field owner and cleaner re-run behavior than imperative create-or-update logic.

### 6. Real host-level liveness proof

The success condition is not just “pod exists.”
It is “`curl localhost:7833` works from the host.”

That proves:

- kind cluster creation worked
- port mapping worked
- borq applied the workload and Service correctly
- the management cluster is externally reachable in the way this first slice promises

## What The Tests Do

### Unit tests

`business/managementbootstrap/managementbootstrap_test.go` checks:

- `DefaultRequest()` returns the expected management bootstrap defaults.
- Empty fields are defaulted correctly by `withDefaults()`.
- Invalid requests fail with the expected error text.
- The rendered default kind config includes:
  - `kind.x-k8s.io/v1alpha4`
  - `hostPort: 7833`
  - `containerPort: 31833`
- `hasCluster()` correctly detects whether a cluster name appears in `kind get clusters` output.

These are fast tests that validate the command’s defaulting and config-shaping logic.

### Integration test

`business/managementbootstrap/integration_test.go` is a real end-to-end bootstrap test.

It:

- Skips cleanly if `kind` is missing.
- Skips cleanly if `docker` is missing or the Docker daemon is unavailable.
- Creates a disposable kind cluster named `borq-management-it`.
- Runs `Execute(...)`.
- Verifies that bootstrap succeeds.
- Runs `Execute(...)` a second time.
- Verifies that the second run succeeds and reports `Reused == true`.
- Deletes the disposable cluster during cleanup.

This test proves the real workflow instead of only mocking it.

## What Is Not Implemented Yet

Still deferred:

- borq server installation into the management cluster
- Vault, ESO, Tailscale, Prometheus, Loki, Grafana, CAPH
- workload cluster bootstrap
- release automation / packaged CLI distribution
- broader `borq plan`, `borq deploy`, and secret-management flows

## Current Verification

The current implementation has been verified with:

- `go test ./...`
- `go run ./cmd/borq --help`
- `go run ./cmd/borq management bootstrap --help`

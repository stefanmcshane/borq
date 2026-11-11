# Borq management bootstrap

The component installer lives in
[`business/cli/management`](../../business/cli/management), and server-owned Helm SDK
setup lives in [`business/server/platform`](../../business/server/platform).
This directory contains support documentation, not runtime Flux manifests.

Kubernetes must already be running. Install k3s and prepare its networking outside
Borq, then use these independent operations:

1. `borq management bootstrap NAME --kubeconfig PATH --api-url URL` installs Borq's
   namespace, RBAC, persistent SQLite volume, Services and server Deployment into
   existing Kubernetes. Configured credentials or the local k3s kubeconfig can be used
   when `--kubeconfig` is omitted. The default native API address is
   `http://localhost:31833`; choose an already reachable URL for remote clusters.
   The server then manages OpenBao, management ESO, storage and monitoring in
   `borq-mgmt`, plus shared cert-manager and Traefik releases in `borq-system`, through
   the Helm Go SDK. No Helm executable is required. Bootstrap waits for component readiness
   and reports progress. Production components are persistent; `--development`
   selects development components, including ephemeral OpenBao and RustFS.
   For an externally prepared development k3d cluster, `--server-image-archive PATH`
   imports an offline server image. Existing saved local credentials remain supported.
2. `borq cluster link NAME --organization ORG --kubeconfig PATH` registers an existing
   deployment target through Borq's API. Linking creates a space named `NAME` bound
   to that cluster. The CLI previews missing or changed target components and queues
   cert-manager, ESO, Traefik and Alloy. `--reconcile=false` previews without enqueueing;
   combine it with `--diff=false` to save only the registration. A failed preview does
   not undo linking. The management cluster already has shared ingress and certificate
   controllers; linking it reuses those releases and adds workload ESO, Alloy and the
   snapshot controller. Organization and space registration remain explicit. `--local OTHER_NAME` remains supported for legacy saved credentials.

`make dev` builds and imports the current image, creates or reuses bare k3d Kubernetes,
then runs minimal bootstrap. The running server owns component setup. Mirrord and Air
serve local API and telemetry traffic through that installation. Borq then creates the
dev organization, links the same cluster as a target, waits for its components, and
binds the default space through the local API. `make dev DEV_DETACH=1` leaves hot reload
running in the background; `make logs`, `make k9s`, and `make dev-stop` inspect or stop
that session. See the
[main development instructions](../../README.md#development-and-deployment-testing)
for published ports, private kubeconfig location, and commands.

Create an organization with `borq org create NAME`, then link its existing clusters.
Every space is bound to a cluster. Linking creates the initial same-named space;
additional spaces can be created with `borq space create NAME --organization ORG
--cluster CLUSTER`. Namespace and certificate issuer defaults are managed by Borq.
Controllers and credentials are shared across a cluster's spaces. User permissions
and cross-namespace network policy are deferred. A physical target ownership marker
prevents a second registration from replacing shared controllers and credentials.
Bootstrap and linking both reject conflicting shared controllers instead of installing
a second copy. Stock K3s Traefik in `kube-system` must be disabled or migrated before
Borq installs its own Traefik release; converged linking only reuses Borq releases.

Client commands use `--api-url` to select Borq. The server configures component
addresses during reconciliation: ESO connects directly to management OpenBao on
NodePort 30200, and Alloy writes to the ingestion-only listener on NodePort 31834
(container port 7834). Deployment targets need routes to that management host;
local k3d targets can share the management Docker network. Each target uses a separate
scoped vault token and receives its own telemetry key through ESO. The ingestion
listener has no administrative API routes. Component credentials stay in OpenBao and Kubernetes
Secrets. Management also uses ESO for its own component credentials. ExternalDNS,
DNS delegation, public TLS routing, and certificate issuers remain deferred.

`--profile NAME` selects named client defaults, independent of cluster provisioning
and component settings. Create one with `borq config set --profile NAME --api-url URL`,
select it for a command with `borq org list --profile NAME`, or activate it using
`borq config use NAME`. Profiles live beneath `~/.config/borq/profiles` (or the
configured `XDG_CONFIG_HOME`) and can contain API, organization, space, and cluster
defaults. Cluster runtime and credential paths are listed in `~/.config/borq/clusters.yaml`,
and default credentials are files named after their clusters in `~/.config/borq/kubeconfigs`.
`config list` includes the base `default` profile, `config current` prints the selected
name, and `config show` groups its resolved settings beneath that name.
Link, components, reconcile, and bootstrap use the selected cluster when no name is
provided. Space creation requires an explicit name and uses the selected organization
and cluster unless flags override them; a missing organization or cluster is an error.
`space list` lists the selected organization's spaces. Application commands follow the selected
space's stored binding rather than changing targets with the client cluster setting.

Development uses ephemeral OpenBao dev mode and small persistent local volumes.
RustFS provides development Loki object storage. Production uses persistent standalone
OpenBao, initialized and unsealed by the server using recovery material retained in
`borq-mgmt/borq-openbao-admin`; RustFS and OpenBao dev mode are excluded. The local-disk
baseline is not HA. Component mode changes require a separate installation or data migration.

Borq owns pinned chart values. Normal server startup and target reconciliation install
missing releases without silently changing existing managed values or versions.
`borq management upgrade --api-url URL` and `borq cluster upgrade NAME` ask the running
server to apply catalog changes in their separate upgrade dependency orders. Target
reconciliation is asynchronous: use `cluster diff NAME` for a read-only comparison,
`cluster reconcile NAME` to queue setup, and `cluster components NAME` to inspect the
saved run. Linking previews and queues without waiting for installation. Reconcile and
upgrade wait by default; `--wait=false` returns after enqueueing, and `--timeout` only
stops local polling. Server work continues after a disconnection or restart. The diff
appears once on stderr and structured status appears on stdout. Failed runs retain their
status and expose `nextRetryAt`; automatic retries back off from 15 seconds to five
minutes. Explicit reconciliation can request a retry sooner. Conflicting component
installations block setup rather than creating duplicates. Management upgrade remains synchronous. Helm
release history and component status persist in Kubernetes and SQLite. These commands
do not upgrade Kubernetes.

After linking, run `borq cluster reconcile NAME --organization ORG` to wait for the
target setup before planning applications. With infrastructure management enabled,
application planning rejects targets whose saved component state is not ready.

`make test-integration-bootstrap` prepares isolated bare k3d test clusters externally, installs Borq,
checks that management startup does not implicitly link a target, and then explicitly
links a separate target. Setup runs through Borq commands with an isolated HOME,
empty PATH, automatic private k3d installation and image import, and saved local
credentials. It verifies unchanged externally configured API publication and
load-balancer mappings across bootstrap, named client profiles, one-URL bootstrap without component URL flags,
automatically managed component addresses, cross-cluster ESO with pinned secret
versions, real app deployment, independent
spaces with shared controllers, authenticated telemetry, RustFS storage, repeated
installation/upgrades, and persistence after a Borq restart. It never installs k3s on
the test host. See the [main README](../../README.md#management-and-target-components)
for pinned versions, upgrade order, storage defaults, and API endpoints.

Existing Flux/Gitea installations need an explicit migration. Bootstrap refuses legacy
resources without deleting controllers, repositories, or volumes. Flux is not used by
the new installation path; existing `gitops` definitions are reference material only.

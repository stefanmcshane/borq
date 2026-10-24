# Borq application manager

Borq expands partial application configuration into a complete desired configuration,
renders a small embedded Helm chart, and applies the saved Kubernetes manifests.
SQLite stores the configuration, immutable plans, resource comparisons, and rollout history.

Organizations contain **spaces** and **registered clusters**. Each cluster registration
creates an initial space with the cluster’s name in the same organization. Every space
must be bound to a registered cluster at creation. Borq assigns its namespace and uses
the `borq` certificate issuer automatically. Spaces have independent app state,
rollout history, queues, and named environment groups; multiple spaces can share a cluster.
Traefik, ESO, cert-manager, and Alloy are shared per cluster. Namespaces separate
resources; cross-namespace network policy and user permissions are future work.
The `apps.yaml` file contains no organization, cluster, or space information.

## Development and deployment testing

Build the local Apple Silicon macOS bundle with `make build-macos-bundle`. The archive
is `dist/macos/borq-local-dev-arm64.tar.gz`, with checksum
`dist/macos/borq-local-dev-arm64.sha256`. Extract it and run
`./borq --help`. Keep the included `borq-server-linux-arm64.tar` beside the executable;
development bootstrap can import it with `--server-image-archive`. These local builds are not notarized
and require macOS 13 or newer.

Borq assumes a k3s cluster is already running. Install Kubernetes and configure its
networking separately, then bootstrap Borq on it:

```sh
borq management bootstrap management --kubeconfig /path/to/k3s.yaml --api-url http://management-node:31833
borq config set --api-url http://management-node:31833
```

Bootstrap installs Borq's namespace, RBAC, persistent SQLite volume, Services and
server Deployment. The running server installs and manages OpenBao, management ESO,
Prometheus and Loki through the Helm Go SDK. No Helm executable is required.
Production components persist their data. Readiness waits for setup, and bootstrap
reports pod readiness, storage and component progress while waiting. Repeating
bootstrap preserves the cluster and its state volume, and can update the server image.
Bootstrap leaves organization creation and target linking to explicit later commands.

`--kubeconfig` selects existing Kubernetes; configured `BORQ_KUBECONFIG` or
`KUBECONFIG` credentials are also supported. On a k3s server, the standard
`/etc/rancher/k3s/k3s.yaml` is used when no other credentials are selected. Saved
legacy Borq cluster credentials remain readable. `--api-url` identifies the already
reachable Borq API endpoint; the default for native k3s is `http://localhost:31833`.
Prepare host port mappings and firewall rules separately.

Bootstrap pulls `ghcr.io/stefanmcshane/borq:latest` unless `--server-image TAG` or a
full image reference selects another image. For an externally prepared development
k3d cluster, `--development --server-image-archive PATH` imports an offline server
image. Keep an existing cluster's component mode consistent when rerunning bootstrap.

For local development, install Go, Docker, kubectl, Air, and Mirrord. Install Stern
for the combined log view, and k9s for the cluster UI. On macOS install the tools
with `brew install go-air stern k9s kubectl` and
`brew install metalbear-co/mirrord/mirrord`.

```sh
make dev DEV_DETACH=1
make logs
make k9s
```

`make dev` builds the CLI and a matching local server image. Its development scripts
prepare disposable k3d Kubernetes, then run management bootstrap against that existing cluster. It starts Air through Mirrord, creates
the `dev` organization, links the same cluster as a deployment target, waits for its
components, and binds the organization's `default` space. The selected client profile
is `development`, so normal Borq commands target this local setup. Without
`DEV_DETACH=1`, the command stays attached to local output; Ctrl-C stops hot reload.
Detached startup returns as soon as its background worker starts; bootstrap and
component installation continue in the background. Use `make logs` to follow progress
or `make dev-wait` to wait for the local API and target components to be ready.
Repeated calls reuse the cluster; a running or starting session is left alone.

The default cluster is `management`, with its kubeconfig and runtime files under
`tmp/management`. Air owns the persistent dev database in `tmp/management/data`;
organization, target, and space setup run through the local API after Mirrord attaches.
The bootstrap pod has its own database and serves again when hot reload stops. Dev
records remain in the local database and return on the next `make dev`. Management
components and deployed applications continue running in the cluster.

Development charts install a namespace-scoped pod history job through the External
Secrets chart. Every five minutes it keeps the five most recent failed pod records
per Helm release, shared across that release's components and chart versions. It
uses controller ownership to identify releases and leaves running, successful, and
unowned pod records alone. Production charts do not install this job. `make test-unit`
also runs its retention tests and requires Python 3.

Mirrord steals API and telemetry traffic from the Borq pod into local ports 27833
and 27834. Requests to `localhost:7833` reach the local server. There is no registry
or image rebuild for each edit: Air rebuilds and restarts the server automatically.
`make logs` follows the local Borq session output alongside Stern's logs from only
the Borq server container in the `borq` namespace, including new and restarted Borq
pods. Ctrl-C closes this view without stopping the dev session. `make kubeconfig`
prints the development kubeconfig's absolute path, and `make k9s` calls it to select
the config for k9s.

For a command prefix that works in any project with a `make kubeconfig` target,
add this to your Bash or Zsh aliases:

```sh
_mk() (
    if [ "$#" -eq 0 ]; then
        printf 'Usage: MK <command> [args...]\n' >&2
        exit 2
    fi
    KUBECONFIG="$(make --no-print-directory -s kubeconfig)" || exit
    export KUBECONFIG
    "$@"
)
alias MK='_mk '
```

Then use `MK kubectl get pods`, `MK k get pods` (if `k` aliases `kubectl`), or
`MK k9s` from the project directory. The trailing space in the alias allows your
shell to expand aliases such as `k`. `KUBECONFIG` applies only to that command;
if `make kubeconfig` fails, the command does not run.

```sh
make dev-stop
```

This stops the local processes, including an in-progress bootstrap, and retains
the cluster and local data. To remove the development setup completely:

```sh
make dev-kill
```

This stops hot reload, deletes the selected k3d cluster and its storage, and removes
the runtime directory (`tmp/management` by default), including the local Borq
database. The next `make dev` starts fresh. Change the
cluster, organization, space, profile, or runtime directory with `DEV_CLUSTER`,
`DEV_ORGANIZATION`, `DEV_SPACE`, `DEV_PROFILE`, and `DEV_DIR`. App HTTP/HTTPS ports
are 8080/18443; use `DEV_HTTP_PORT` and `DEV_HTTPS_PORT` if those ports are occupied
when creating a fresh cluster. Reusing a cluster preserves its published ports.
Startup checks these ports before building images and reports conflicts with an
override to use. The HTTPS default avoids the older app test cluster's port 8443.
`DEV_API_PORT` overrides the host API port, which defaults to 7833.
Use the same overrides for subsequent `make logs`, `make kubeconfig`, `make k9s`, `make dev-stop`, and `make dev-kill`.
For `MK`, set overrides on the prefix, for example `DEV_DIR=tmp/custom MK k9s`.
For CLI development, use `go run ./cmd/borq ...` from another terminal so commands
run the current checkout rather than an older `./borq` binary.

`make test-dev` exercises detached startup, repeated start, the combined log view,
Air reload, application rollout, port conflicts, startup cancellation, restart persistence,
complete cleanup, and a fresh start using a separate cluster
and isolated CLI configuration. It needs the same development tools plus Stern and
free ports 17833, 18080, 19443, 27833, and 27834. Stop local hot reload before running
this test. It removes its cluster and temporary files afterward;
`DEV_TEST_KEEP=1` retains them for debugging after stopping local hot reload.

`make test-integration-bootstrap` builds the current image and creates isolated k3d
management and deployment target clusters. It checks bare creation before installation,
server-owned SDK setup, explicit later target linking, cross-cluster ESO access,
scoped credentials, real app rollout, private tool installation with an empty PATH,
automatic image loading and local credential discovery,
repeat installation, explicit upgrades, independent spaces on a shared target, and SQLite persistence. Test clusters are
removed afterward; set `BORQ_INTEGRATION_KEEP_CLUSTER=1` to retain them for debugging.
It does not install k3s on the host or change the default kubeconfig.

The older app acceptance fixture remains separate from the management bootstrap:

```sh
make dev-cluster
make test-apps-e2e
```

These targets use `borq-dev` and `tmp/dev/kubeconfig`, with Kubernetes at
`127.0.0.1:6550`, HTTP at `127.0.0.1:8088`, and HTTPS at `127.0.0.1:8443`.
`dev-certificates` and `dev-secrets` maintain that legacy test fixture; they are not
part of the management installation workflow. The suite tests YAML and manifest
round trips, partial updates, drift correction, saved plans, env version pinning,
predeploy gates, org/cluster/space discovery, space isolation, restart recovery, and successful and failed CLI exit codes.
Its temporary namespaces, servers, and databases are cleaned up afterward.
HTTPS tests use a SelfSigned ClusterIssuer and trust its test certificate without
changing public DNS. Set `DEV_KUBECONFIG_PATH` and
`BORQ_APPLICATION_TEST_INGRESS_URL` for another fixture.

## Run and deploy

Bootstrap starts the Borq API; `make dev` serves it through the local process.
Use the CLI from another terminal (`borq` below denotes a built binary):

```sh
borq org create example
borq org list
borq cluster link local --organization example --kubeconfig /path/to/k3s.yaml
borq cluster reconcile local --organization example
borq cluster list --organization example
borq cluster kubeconfig local --organization example --output ./local.kubeconfig
borq space list --organization example
borq plan -f apps.yaml --organization example --space local
borq apply -f apps.yaml --organization example --space local --wait
```

`cluster link NAME` requires an organization from flags or client settings. It registers
an existing cluster and creates its initial space named `NAME`. Supply `--kubeconfig PATH`
for the cluster's credentials. `--local OTHER_NAME` remains available for saved legacy
local credentials. Bootstrap does not link its cluster automatically.
The CLI reads and flattens
the kubeconfig, including referenced certificates,
and uploads it to the API. Borq stores the credentials privately with mode 0600;
existing registrations reject different credentials. Kubeconfigs using exec credential plugins
require the plugin and its credentials on the server; embedded certificate or token
credentials avoid that dependency.
The Kubernetes API address must be reachable from the Borq server. For an in-cluster
server, a kubeconfig address on `127.0.0.1` points at Borq's own pod. For locally created
clusters, Borq discovers a control-plane node's internal IP through Kubernetes and
uses port 6443 in the uploaded credentials, leaving the local kubeconfig unchanged.
Externally prepared development targets can share their management cluster's Docker network.
External kubeconfigs must already contain an address reachable from the Borq server.
`cluster kubeconfig [NAME]` downloads a kubeconfig through the management API, using the
selected profile's organization and cluster when omitted. It prints kubeconfig YAML
to stdout, or writes a private file with `--output PATH`, without changing the default
Kubernetes context. New links preserve the original client API address separately
from the address used by the management server. `--api-server URL` overrides the
downloaded address when connecting from another network or using an older registration.
The JSON API exposes this as
`GET /api/v1/organizations/{organization}/clusters/{cluster}/kubeconfig`, returning
`{"kubeconfig":"..."}`. Only this dedicated endpoint returns credentials; lists contain
metadata. Responses disable caching. Access permissions for this endpoint are future work.
`cluster link` first saves the registration, then prints a read-only summary of missing,
changed, and already-installed target components to stderr. `--verbose` includes the
full values diffs; `--output json` or `--output yaml` returns the structured registration
or queued state on stdout. By default it prints a short confirmation and returns after
queueing setup, without waiting for installation. A failed preview leaves the cluster
linked and schedules no new work.
Existing component installations are checked for conflicts so Borq does not silently
install duplicates in another namespace. Use `--reconcile=false` to preview without
queueing setup, or `--reconcile=false --diff=false` to save only the registration.
Inspect progress with `cluster components`, or use `cluster reconcile` to queue and
follow setup. Deployment plans require the target's saved component state to be ready
when infrastructure management is enabled. Pass `--api-url URL` to connect to another
Borq server.
If the CLI cannot contact the selected API, it reports the API URL and recovery
steps instead of a raw connection error. Local setup guidance checks the existing
k3s cluster and points to management bootstrap with its kubeconfig. Remote failures direct you to server reachability and API selection;
DNS and certificate failures have their own guidance. Use `borq config show` to
check the selection and `borq config set --api-url URL` to change it, adding the
same `--profile NAME` when using a named profile. Failed writes are not automatically
retried by this error handler.
Targets should be fresh or already managed by this Borq installation. Existing
Traefik, ESO, or cert-manager releases in other namespaces are not adopted; migrate
those installations before registering the target.
Detection uses Helm release records and standard component labels on controllers;
custom installations without either may need manual inspection.

The CLI creates `~/.config/borq/config.yaml` on first client use, or
`$XDG_CONFIG_HOME/borq/config.yaml` when `XDG_CONFIG_HOME` is set. It stores:

```yaml
server: http://127.0.0.1:7833
organization: ""
space: default
```

Select defaults once, then omit the corresponding flags:

```sh
borq config set --organization example --space default
borq config show
borq space list
borq apply --wait
```

`config set --api-url URL` saves another API address. Named client profiles can hold
API, organization, space, and cluster defaults:

```sh
borq config set --profile team-west --api-url https://borq.example.com --organization example
borq org list --profile team-west
borq config show --profile team-west
borq config use team-west
borq config list
borq config current
```

`--profile NAME` selects a saved client profile for that invocation; `config use`
changes the active profile. `config list` includes the base settings as `default`,
and `config use default` returns to them. `config current` prints just the selected
profile name. `config show` groups the resolved settings under that name:

```yaml
default:
  cluster: local
  organization: demo
  server: http://localhost:7833
  space: default
```

Named profiles live in
`~/.config/borq/profiles/NAME.yaml`, beneath the same optional `XDG_CONFIG_HOME`.
Profiles do not choose a Kubernetes runtime or management component mode.
When their name is omitted, `cluster link`, `cluster components`, `cluster reconcile`,
and `management bootstrap` use the selected profile or base cluster. An explicit name
overrides that selection. Application commands use the selected space's stored
cluster binding; changing the client cluster does not redirect a deployed space.
Explicit `--api-url`, `--organization`, and `--space` flags override saved settings for that command only.
Changing the saved organization resets its space to `default` unless `--space` is
supplied too. The directory uses mode 0700 and the YAML file mode 0600; edits replace
the file atomically. Settings can also be edited directly. Local cluster runtime and
credential metadata is stored separately as a YAML list in `~/.config/borq/clusters.yaml`:

```yaml
- name: local
  runtime: docker
  kubeconfigPath: /Users/you/.config/borq/kubeconfigs/local
- name: target
  runtime: docker
  kubeconfigPath: /Users/you/.config/borq/kubeconfigs/target
```

Default kubeconfigs live in `~/.config/borq/kubeconfigs`, with each cluster name as its
filename. Custom `--kubeconfig-output` paths remain recorded in the registry.
Registry updates are locked and written atomically so concurrent cluster creations
retain each other's entries. Credentials and registry files use mode 0600.
Older `~/.borq/clusters.yaml` and `~/.borq/clusters/NAME/cluster.yaml` records are
imported automatically when local clusters are accessed. Their default kubeconfigs
are copied into the shared config folder with certificate, key, and token files
embedded. Private k3d installations live under `~/.config/borq/bin`. Original files remain as
backups, and existing registry entries take precedence over those backups.

Borq only writes its kubeconfig files to the locations above. It does not edit
`~/.kube/config`, merge credentials into a user's kubeconfig, switch kubectl contexts,
or change shell settings. Users decide how to load these files in kubectl and other
tools. Development commands select their kubeconfig only for the processes they
launch, using explicit flags or a process-local environment.

`apply` needs an organization and space from settings or flags, or an explicit
`--plan ID`. It reads `apps.yaml` by default. Changing settings after planning does
not redirect a saved plan: applying the sidecar requires its original server,
organization, and space, or a new plan.
Create the organization and link a cluster explicitly. Linking creates a bound space
with the cluster’s name. Create additional spaces with `space create NAME --cluster CLUSTER`. Omitting
`--cluster` uses the selected profile or base cluster. Space creation requires an
explicit name, an organization, and a cluster; organization and cluster flags override
the profile's defaults. `space list` lists only the selected organization's spaces,
with `--organization ORG` overriding the profile. Planning requires a cluster binding. The target
namespace name is allocated when the space is created; the namespace itself is created
during apply. Namespace and certificate issuer settings are managed by Borq and are
not exposed as space configuration in the API, MCP, CLI, or UI.
Neither plan nor apply automatically creates organizations, clusters, or spaces.

Read all accepted apps in the selected space as expanded API YAML, or export their
exact saved Kubernetes manifests:

```sh
borq apps list
borq apps list --manifest
borq apps list --organization example --space development
```

Both views come from the latest accepted apply, including queued or failed rollouts.
Planning alone does not change the output. Reads use saved state and do not contact
the target cluster, resolve newer env versions, or create a plan. The default output
can be saved as `apps.yaml`; `--manifest` prints Kubernetes YAML documents separated
by `---`. An empty space returns `apps: []`, with no manifest documents.

Cluster registrations are scoped to the organization that created them. A physical target
cluster is owned by one registration; registering it again under another organization
or name is rejected before shared controllers or credentials can be changed.

The client also stores the private plan-reference sidecar beside the app file.
Application files and expanded app views reference environment groups by name and version;
they do not contain literal env values. Authorized environment-group API and CLI reads return
full values. Rendered Helm values and Kubernetes manifests contain pinned Secret references.
The application client does not load a `.env` file or infer target flags from environment variables.
The older `BORQ_*` runtime settings configure server startup, rather than selecting
an application deployment target.

See [the frontend/backend example](business/server/apps/apps.yaml). New apps require
an existing container image; builds are outside this manager. App names are limited
to 50 characters. Replicas default to one.
Apps with a port receive a private Service, and named ingresses add public Traefik routes.

Deployment plans supply organization, space, cluster, and application ownership as
Kubernetes labels and annotations. These are generated by the server and do not
belong in `apps.yaml`:

| Key | Value |
| --- | --- |
| `borq.run/organization-id` | The unique organization name, currently its identifier |
| `borq.run/organization` | Organization name |
| `borq.run/space-id` | Stable space ID |
| `borq.run/space` | Space name |
| `borq.run/cluster-id` | Stable registered cluster ID |
| `borq.run/cluster` | Registered cluster name |
| `borq.run/app-id` | Stable app identity within its space |
| `borq.run/app-name` | App name |

Deployments, their ReplicaSets and pods, Services, Ingresses, predeploy Jobs and
pods, ExternalSecrets, and generated environment and TLS Secrets carry the app's
ownership metadata. Namespaces carry organization, space, and cluster metadata.
Existing Service and Deployment selectors keep their original app and target
labels. Applying a new plan updates namespace ownership metadata and restores
managed labels and annotations removed manually, while retaining unrelated namespace
metadata. Saved plans retain their exact metadata; create a new plan to add these
fields to an older deployment.
The standalone render API accepts optional `scope` with `organization`, `space`,
and `cluster` names. Rendering a saved plan already includes its original scope.
Readiness and startup checks live under `checks` and default to `/healthz` on the app port, with independent
overrides. A port of zero disables networking and the default checks. Existing resolved
settings are retained during partial updates, including probe ports when only an app's
listener port changes.

All named ingresses request HTTPS automatically. The app definition contains only
the route's `name`, `host`, and optional `path`; certificate Secret names are generated
inside the renderer. Every new space uses the cert-manager ClusterIssuer `borq`
automatically. The issuer is frozen into each plan's render values
and generated Ingress annotations, outside the application spec.

Target installation supplies Traefik's `websecure` entrypoint and cert-manager.
Configure an appropriate ClusterIssuer separately; DNS01 issuers also need DNS-provider
credentials. ExternalDNS, delegated DNS, wildcard domains, and issuer provisioning are
not installed yet. Borq generates routing annotations and TLS references, but these
do not create public DNS records or configure a certificate authority.
The Borq server needs read access to Certificates and TLS Secrets in managed namespaces
to observe HTTPS readiness; it does not own or save those controller-generated resources.

`command` is one string containing an executable and its arguments. Borq parses
shell-style quoting, including empty quoted arguments, then starts that process
directly. An empty string uses the image defaults. Use an explicit shell for shell
expressions, for example `command: sh -c 'prepare && exec ./app serve'`. The expanded
configuration normalizes command quoting so manifest readback produces the same value.

```yaml
apps:
  - name: backend
    image: ghcr.io/example/backend:v2
    port: 8080
    command: ./app serve --port 8080
    checks:
      readiness:
        path: /ready
      startup:
        path: /started
```

## Predeploy tasks

An app can define one unnamed predeploy process that must finish before its
space's applications are updated. It runs as a Kubernetes Job in the space
namespace using its app's current image and environment variables, with its own command.
An empty command disables predeploy; an omitted predeploy object starts disabled.

```yaml
apps:
  - name: backend
    image: ghcr.io/example/backend:v2
    port: 8080
    predeploy:
      command: ./app migrate
      timeoutSeconds: 300
      backoffLimit: 0
```

Tasks run in app-name order. Every task must complete successfully before
Borq applies any application's Deployment, Service, or Ingress in that space.
An ordinary webserver can be the task process, but it blocks until it exits successfully;
HTTP readiness does not complete a predeploy task. The task deadline defaults to 300
seconds and Kubernetes retries default to zero. The overall server rollout deadline
also covers Secret synchronization and predeploy work. It starts when a queued apply
begins execution and retains that execution deadline across server restarts; time waiting
in the queue does not consume it.

The saved plan includes the exact Job manifests. Each new rollout uses distinct,
deterministic Job names ending in `-pre`, with a bounded app prefix and a rollout
identifier to stay within Kubernetes naming limits. Retrying the same plan or recovering
after restart reuses those Jobs. Borq checkpoints the completed phase before updating applications.
On failure or deadline it stops the rollout and suspends unfinished tasks; Kubernetes
terminates their pods asynchronously. Graceful Borq shutdown leaves the Jobs running
so another server start can resume observation. Task commands should tolerate execution
retries; Kubernetes Job execution is not an exactly-once guarantee.

All configured tasks in the expanded space run for each new rollout, including
tasks on apps omitted from a partial update. Supplied predeploy fields merge into the
stored object. An explicit `predeploy: {command: ""}` disables future executions;
historical Jobs remain for inspection. Disabled settings remain in the expanded config
and in Deployment annotations so manifest readback preserves their execution budgets.
For manifest readback, supply the Jobs from one plan alongside its application resources,
as combining historical Jobs from several plans is ambiguous.

## Named environment groups

Applications reference separately published environment groups. Inline values in an app's
`env` field are rejected. The reference has two string fields:

```yaml
apps:
  - name: backend
    image: ghcr.io/example/backend:v2
    port: 8080
    env:
      name: backend-settings
      version: latest
```

For a new app, omitting the config name selects `<organization>-<space>-<app>`, scoped
to the target space, and omitting the version selects `latest`. Existing app
updates retain their selector when the whole `env` field is omitted. Supplying `env`
without a version selects `latest`, including `env: {}` or the same config name.
For example, backend in organization
`example` and space `development` defaults to `example-development-backend`.
The app image version and environment group version are independent.

**Planning resolves `latest` to an existing published version and saves that concrete
version.** Expanded app views and diffs contain config names and versions, not values.
A saved plan referencing version `"8"` keeps using version 8 even if version 9 is
published before apply. Neither plan nor apply publishes a config version or predicts
which version a future write will receive. Only the latest and ten previous versions
can be selected or deployed: with latest version 20, versions 10–20 are allowed and
version 9 is rejected. Borq checks this against the current vault head during planning,
before accepting apply, when queued work starts, and before updating Deployments after
predeploy. A saved plan that ages out fails instead of changing its pin. This limit is
enforced even when OpenBao still holds the older version or a Kubernetes Secret exists.

An implicitly selected default config that has never been published resolves to
version `"0"`: an empty environment group with no ExternalSecret or generated Secret.
This lets an app with only its name, image, and port deploy without vault setup.
An explicitly named config, or a requested positive version, must resolve successfully;
missing published versions return HTTP 404. Missing store access or credentials fail
planning. Version 0 is reserved for empty configuration and is not an OpenBao version.

Publish a complete value map in a separate file, for example `env.yaml`:

```yaml
values:
  - name: LOG_LEVEL
    value: info
  - name: DATABASE_URL
    value: postgres://database/shop
  - name: OPTIONAL_VALUE
    value: ""
```

```sh
borq environment-group create backend-settings -f env.yaml --organization example --space development
borq environment-group get backend-settings --organization example --space development
borq environment-group get backend-settings --version 1 --organization example --space development
borq environment-group list --organization example --space development
borq environment-group clone backend-next --from backend-settings --version 1 --organization example --space development
```

`environment-groups` and `env` are aliases for `environment-group`.

Publication replaces the complete config map. A key omitted from `values` is absent
from the resulting version; `values: []` publishes an empty map. Publishing the same
values reuses the current version. Changed values create a new OpenBao KV v2 version,
retaining the latest plus ten previous versions (eleven total) per config family.
Use `environment-group create --base-version N`
to require that N is still the latest version; zero requires a new family. Concurrent
publication uses check-and-set, and conflicts return HTTP 409 instead of losing changes.
Cloning creates a new family from the selected source version without deploying it.

Editing or publishing a config never updates running applications. The next plan using
`latest` selects the new version; explicit version references stay pinned. To prepare
variables for one particular image change, clone a config to a different name and
reference that name in the image's application update. Omitted app fields retain their
previous selectors, so an app that selected `latest` continues selecting latest on
future plans, even though each individual plan saves a concrete version.

With infrastructure management enabled, target registration installs ESO and a
`ClusterSecretStore/borq-openbao` pointing directly to management OpenBao's managed address.
Each target gets its own KV v2 mount and scoped token, stored in `borq/borq-openbao`,
key `token`, on that target. Environment group documents are scoped within that mount at
`borq/environment-groups/<space-id>/<group-name>`. Existing groups retain their original
vault paths and version history when upgrading from the previous name. Borq accesses the provider selected
by the store; OpenBao does not need to run in the deployment cluster.

The management vault runs in `borq`. Development uses in-memory OpenBao dev, so
restarting that vault loses its contents even though the Borq SQLite database remains.
Vault tokens are periodic with a 24-hour period and are renewed on server startup and
every six hours. Borq keeps the per-cluster vault and telemetry credentials in management
OpenBao. Organization-specific vault selection remains deferred.

The standalone application server defaults infrastructure management to disabled:
`go run ./cmd/borq server --data-dir ./tmp/apps --port 7833` still supports externally
installed dependencies. That legacy test setup uses an in-cluster store and token in
`borq-system`. Runtime app apply itself does not install components; bootstrap and
cluster registration own component installation.

Each positive config version renders a separate ExternalSecret and an immutable,
Borq-named Kubernetes Secret. The ExternalSecret pins the existing KV version and uses
`OnChange` refresh. Reapplying the same config version reuses its Secret identity;
selecting another config or version changes the reference. Apps and predeploy jobs use
the same snapshot. Borq waits for ESO readiness and checks that the generated Secret
exists with the expected identity and keys before starting predeploy or updating Deployments.

Old ExternalSecrets and Kubernetes Secrets remain; cleanup is deferred. OpenBao's
retention limit does not remove those existing Kubernetes Secrets, but a pruned vault
version cannot be fetched or recreated. Publish or clone important configurations into
a separate family before they age out if they must remain available for future planning.
Authorized environment-group API and CLI reads return raw values; application render and inspect
round trips need only the config reference and never reconstruct a literal env map.

## Plans, queued applies, and updates

- Omitted apps, fields, and named ingress entries retain their stored values. Named apps
  and ingresses merge by name; command strings replace in full, and env reference fields
  select a config family and version.
  Empty app or ingress lists clear the corresponding configuration, subject to the deletion restriction below.
- Planning merges first, adds defaults, and resolves env versions. It returns all apps in
  the space. `--manifests` prints the Kubernetes resources instead of the expanded app view.
- Each saved plan includes a configuration diff and a separate live Kubernetes comparison.
  Live comparisons use server-side dry-run and normalize runtime bookkeeping while
  preserving meaningful configuration differences. These comparisons describe what was
  observed during planning; they do not reserve resources or lock the space.
- The CLI saves a private `<file>.borq-plan.json` sidecar. A matching unapplied plan
  is applied exactly as reviewed. A changed file or target makes ordinary `apply`
  create a fresh plan. Without a matching plan, `apply` prints the configuration diff and changed resource
  actions to stderr, then submits that exact plan. A completed or failed rollout's
  sidecar does not block the next apply. An unchanged retry follows an active rollout;
  changed input starts a new plan. `apply --plan PLAN_ID` always selects that exact
  existing plan. `--plan-file PATH` also selects an exact plan and rejects changed
  input or scope. Wait progress and blocking resources are reported on stderr,
  including a heartbeat during long waits. Operation output remains YAML on stdout.
- Accepted applies enter a durable FIFO queue per space in server acceptance order.
  Planning does not enter the queue. One space's rollout does not block another
  space's queue. Reapplying an already accepted plan returns its original operation.
- Every queued operation applies its complete saved space snapshot, including apps
  omitted from the original partial update. Borq does not rebase saved plans onto earlier
  queued changes. If A and B were planned from the same starting state, B's later apply can
  restore fields to B's saved values after A changes them. Review a fresh plan when those
  earlier app changes should be retained. Environment group versions remain the ones each plan selected.
- A newer desired revision or changed live cluster state does not reject an accepted saved
  plan. During execution, Borq validates ownership and required resources, synchronizes
  Secrets, runs predeploy, and applies the exact saved manifests. It overwrites manual edits
  to Borq-controlled configuration. Kubernetes defaults and injected sidecars remain outside
  that configuration boundary. There is no continuous drift reconciliation.
- The space records its latest accepted desired configuration when an apply is queued.
  A failed rollout retains that desired state; the last successful revision is recorded
  separately. Failure lets the next queued apply proceed. Active operations and queued
  work survive server restart. Queue cancellation is not implemented.
- The server's rollout deadline starts when execution begins, covering Secret synchronization,
  predeploy, and application readiness. The API's `startedAt` field records acceptance time,
  so it can precede execution. `--wait` polls through queued and active states and returns
  a failing exit code on rollout failure or local timeout; client disconnection does not
  cancel server work.
- Deployment success requires the applied template's replicas to be available and previous
  ReplicaSets scaled down. Managed Ingress success also requires the matching Certificate
  revision to be Ready and a matching, unexpired TLS certificate and private key covering
  the route host. Public DNS propagation and external reachability are not rollout gates.

Deletion commands, pruning, and rollback are deferred. A plan that would remove an
existing Kubernetes resource from its planning baseline is rejected, including clearing
an app list or ingress list that would remove resources. Historical predeploy Jobs and
Secret snapshots are retained. Changing a config's values happens through config
publication rather than an application update. Addons, builds, autoscaling, and MCP
remain deferred.

## API and persistence

The [OpenAPI contract](api/appapi/openapi.yaml) generates the Go HTTP server and client:

```sh
make generate-api
make test-unit
make test-integration-server
make fmt
```

The running server serves the htmx management UI at `/`. Select an organization
and space to manage applications, cluster connections, deployment bindings, and
versioned environment groups. **Applications** lists apps in the selected space;
select an app to open its own **General**, **Configuration**, and **Manifests** pages.
**Add an application** opens the same guided editor used for existing apps. Its YAML
checkbox switches between form fields and a single application document, without an
`apps:` wrapper, while preserving unsaved settings. Configuration and saved manifests
show only the selected app. Application changes create a saved plan for review;
applying it queues a deployment whose progress updates automatically. The guided editor
covers the complete application spec: images, replicas, ports, commands, environment
groups and versions, named HTTPS routes, independent readiness and startup paths,
ports, timing and failure thresholds, and predeploy commands with deadlines and retry
limits. Route cards support adding routes and discarding unsaved rows; deployed route
names remain fixed because resource deletion is not supported yet. Cluster pages show component progress, reconciliation and upgrade controls,
and an explicit kubeconfig download. Management status and upgrades are available only
in the separate Management section, without an organization or space selection.
Converged and separate deployments use the same cluster listing and linking form.

**Environment groups** has a list and a separate page for each group, with
**Variables**, **Versions**, **Applications**, and **Settings** tabs. Create and
publish groups from the variables editor, browse retained versions, see which apps
use a group, and clone a version under a new name from Settings. Applications select
published groups from a dropdown; group creation and cloning use name fields.
Switching between variable fields and YAML preserves drafts and publication guards.
An app without public routes shows its internal service address using the space's
server-assigned namespace. The address preview follows the app name and port and
disappears when networking is disabled.

In **Clusters**, link an existing cluster using its kubeconfig. Linking creates its
same-named space and queues target component setup. Borq manages cluster components
and applications; Kubernetes installation and machine provisioning happen separately.

The UI sends every action through the same validated API used by the CLI and MCP.
Browser mutations have origin protection, and secret-bearing pages and downloads
are not cached. The page, styles, fonts, and libraries are embedded in the binary.
Run `make test-ui` for HTML, form, validation, and asset checks; the real deployment,
environment publishing, and clone workflow also runs in `make test-integration-server`.
`make dev` reloads UI asset changes alongside Go changes.

The UI uses Basecoat's Vega components with a shared charcoal-and-violet theme.
Colors, typography, and radii live in [theme.css](api/webui/assets/theme.css);
buttons, badges, alerts, and empty states use the same component library.
[Shared templates](api/webui/components.html) and [icons](api/webui/icons.html)
keep the shell consistent. The appearance switch defaults to dark and saves the
light/dark preference in the browser. Basecoat and Inter are pinned and bundled
locally alongside htmx.

The running server hosts an interactive API explorer at `/api/docs/` and serves its
contract at `/api/openapi.yaml`. The explorer bundles Swagger UI 5.32.15 from its
[upstream release](https://github.com/swagger-api/swagger-ui/releases/tag/v5.32.15)
with Borq, requires no separate service or CDN, and sends requests to the same
server address used to open the page. Application requests
and responses use JSON. The CLI converts `apps.yaml` into the documented `Patch`
request and prints expanded JSON responses as YAML. It rejects duplicate YAML keys
and multiple documents before posting. The API validates unknown fields and explicit
nulls against the same schema. Targets remain in the URL.

All JSON endpoints now live under `/api/v1/`, the API explorer under `/api/docs/`,
and operational health checks under `/api/`. The old unprefixed API routes are removed.
Keep CLI `--api-url` settings at the server origin (for example,
`http://localhost:7833`); the generated client supplies the API path. Rebuild existing
CLI binaries after this route change. Existing target telemetry configurations need
a component upgrade to adopt the `/api/v1/ingest/` gateway paths.

The server also exposes an MCP endpoint at `http://localhost:7833/api/mcp` using
Streamable HTTP. Tools are derived at startup from the same embedded OpenAPI
contract: each operation ID becomes a tool name, with its summary, description,
and input constraints. Run `make generate-api` after changing the contract, then
rebuild or let `make dev` reload the server. There is no separate tool registry
or MCP process to maintain.

Tool arguments group URL parameters under `path`, query parameters under `query`,
and the JSON request payload under `body`. For example, `PutOrganization` accepts
`{"path":{"organization":"example"}}`, `ListOrganizations` accepts `{}`, and
`ExpandApps` accepts `{"body":{"apps":[{"name":"api","image":"nginx:1.27.5"}]}}`.
Omitted patch fields stay omitted. Results include the API response as text and
structured content containing `status` and `body`; API failures set `isError`.
Plan creation and application remain separate tools, and queued operations return
immediately for polling through `GetOperation`.

The MCP endpoint shares the API's trusted-administrator access boundary and
management readiness checks. It exposes the same administrative actions and
credential reads, so it requires the same restricted deployment access.

The conversion endpoints use the same app model:

| Endpoint | Input | Output |
| --- | --- | --- |
| `POST /api/v1/apps/expand` | Partial app configuration JSON | Complete configuration with explicit defaults and empty lists |
| `POST /api/v1/apps/render` | Configuration, namespace, and target ID | Helm values, renderer version, and Kubernetes manifests |
| `POST /api/v1/apps/inspect` | Rendered or live Borq manifests | Complete app configuration read from resource fields |

Organization and target discovery endpoints are:

| Endpoint | Operation |
| --- | --- |
| `PUT /api/v1/organizations/{organization}` | Create or retrieve an organization |
| `GET /api/v1/organizations` | List organizations |
| `PUT /api/v1/organizations/{organization}/clusters/{cluster}` | Register a cluster in an existing organization |
| `GET /api/v1/organizations/{organization}/clusters` | List that organization's clusters |
| `PUT /api/v1/organizations/{organization}/spaces/{space}` | Create a space bound to a registered cluster |
| `GET /api/v1/organizations/{organization}/spaces` | List that organization's spaces |
| `GET /api/v1/organizations/{organization}/spaces/{space}` | Read a space's binding and current app state |
| `GET /api/v1/organizations/{organization}/spaces/{space}/apps` | Read expanded apps and exact manifests from one accepted snapshot |

Existing SQLite projects and deployment environments migrate to spaces in their
original organization, normally named `<project>-<environment>`. Long names and name
collisions receive a unique suffix; use `space list` to see the resulting names.
Existing target IDs, namespace bindings, manifests, config references, revision history,
and queued operations remain intact. The old project and deployment-environment API
paths are removed.

Environment-group endpoints are scoped under
`/api/v1/organizations/{organization}/spaces/{space}/environment-groups`:

| Method and suffix | Operation |
| --- | --- |
| `GET` collection | List Borq-published environment groups with their latest version and full values |
| `GET /{group}` | Read latest with full values |
| `GET /{group}/versions/{version}` | Read a pinned version, or `latest` |
| `POST /{group}/versions` | Publish complete `values`, with optional `baseVersion` guard |
| `POST /{group}/clone` | Create a group from `sourceName` and optional `sourceVersion` |

The CLI's YAML round trips preserve configuration values, rather than the original
comments or omitted-field formatting. Inspection handles Borq's Deployment, Service, and
Traefik Ingress shapes; it rejects features that cannot be represented by the app
model. Kubernetes runtime defaults and injected sidecars do not become app fields.
Disabled probe settings use annotations because Kubernetes has no probe fields to hold them.
Environment group names and versions are reconstructed from manifest fields and
checked against the pinned ExternalSecret identity without reading raw vault values.
Include the currently referenced ExternalSecrets alongside Deployments and predeploy
Jobs when inspecting live manifests; historical ExternalSecrets are not part of the
current configuration. Other supported app fields
are read directly, so manual changes remain visible.

The API is an administrative
surface for trusted callers; organization grouping does not provide authentication or
tenant authorization in this first version. Run it behind an authenticated private boundary.
The server listens on loopback by default. Use `server --listen 0.0.0.0` when running
inside a cluster or serving remote clients through that private boundary.
`server --rollout-timeout 10m` sets the server's rollout deadline independently of
the CLI's `apply --timeout` wait deadline.

Use one server process per SQLite database on persistent local storage. A process lock
prevents two servers from resuming the same operations. Database, journal, and lock files
have owner-only permissions. Multiple CLI clients can use the API; applies queue per
space, and database transactions never wait on Kubernetes network calls.

## Management and target components

All component charts, pinned versions, and values live in Borq's
[platform catalog](business/server/platform/catalog.go) and
[embedded values](business/server/platform/values). Borq installs and upgrades releases
through the Helm SDK and stores Helm release history in Kubernetes Secrets. No Helm
binary, Flux controller, Gitea service, or Git repository is required. Installed
component state is also recorded in Kubernetes ConfigMaps and the Borq database.

| Location | Component | Pinned chart | Purpose |
| --- | --- | --- | --- |
| Management | OpenBao | 0.30.3 | Central KV v2 secrets and cluster credentials |
| Management | External Secrets Operator | 2.5.0 | Materialize supporting component secrets |
| Management, dev only | RustFS | 1.0.1 | Local S3-compatible development storage |
| Management | Prometheus | 29.36.2 | Metrics storage and remote-write receiver |
| Management | Loki | 7.2.0 | Log storage |
| Target | cert-manager | 1.21.1 | Certificate controller; issuers configured separately |
| Target | External Secrets Operator | 2.5.0 | Materialize app and Alloy secrets from management OpenBao |
| Target | Traefik | 42.0.0 | Application ingress |
| Target | Alloy | 1.12.1 | Collect target logs and metrics |

Components use the `borq` namespace on each cluster. A management cluster can also be
registered as a target; ESO is shared rather than installed as a competing release.
ExternalDNS is deferred until the DNS story is defined. RustFS is development-only:
dev Loki stores its objects in a Borq-created RustFS bucket using credentials supplied
through OpenBao and ESO. Production Loki currently uses filesystem storage.

The baseline uses single replicas, small local volumes, and no HA. Borq's SQLite
volume is 1Gi. The `borq-local` StorageClass uses k3s's local-path provisioner, waits
for a consuming pod, and retains volumes. Dev Prometheus and Loki each use 1Gi with
24-hour retention; production defaults use 5Gi with seven-day retention. Supply
`--storage-class` when an existing cluster uses another provisioner.

### Production and existing clusters

Start with an existing k3s cluster and its kubeconfig:

```sh
borq management bootstrap management --kubeconfig /etc/rancher/k3s/k3s.yaml --api-url http://management-node:31833
borq org create example --api-url http://management-node:31833
borq cluster link management --organization example --kubeconfig /etc/rancher/k3s/k3s.yaml --api-url http://management-node:31833
```

Kubernetes installation, machine access and networking are handled outside Borq.
Management bootstrap installs the Borq server and its supporting components. Linking
an existing cluster enrolls it as a deployment target and creates its initial space.

Production OpenBao uses standalone persistent storage. The Borq server initializes and
unseals it, saving its admin token and recovery material in
`borq/borq-openbao-admin`. This is an initial local-disk baseline, not a production HA
or independently secured recovery design; protect Kubernetes access and back up both
vault data and recovery material. Dev OpenBao remains explicitly ephemeral.

A standalone server enables its own component setup using `--management-enabled` and
target installation using `--infrastructure-enabled`, together with
`--management-kubeconfig` (omit inside the management cluster),
`--api-url` with an advertised management address, `--infrastructure-profile`,
and `--infrastructure-storage-class`. Reconciliation derives component addresses
from that management host. The bootstrapped server already receives these settings.
Its HTTP listeners
start before management setup: `/api/livez` checks local SQLite liveness, while `/api/healthz`
and `/api/readyz` remain unavailable until management components are ready.
`--telemetry-port` selects
its separate ingestion listener (default 7834); bootstrap service overrides use
`--telemetry-node-port` and `--telemetry-host-port`.

### Credentials and telemetry

API client commands select Borq with `--api-url`. The server manages component
networking during reconciliation. Each registered cluster receives a scoped vault
token and a separate telemetry bearer key. ESO connects directly to management
OpenBao on NodePort 30200. Alloy obtains its
telemetry key through ESO and sends logs and metrics through Borq's authenticated
management gateway:

- `POST /api/v1/ingest/{cluster-id}/loki/api/v1/push`
- `POST /api/v1/ingest/{cluster-id}/api/v1/write`

A key authorizes only its matching cluster route. Borq stores only the telemetry
key's digest in SQLite; raw credentials stay in OpenBao and target Secrets.
The public `borq-telemetry` Service selects the separate ingestion-only listener on
port 7834 (NodePort 31834). It has no administrative API routes. Development publishes
this listener on host port 7834 while the administrative API's host port stays on
loopback. Point public telemetry routing at this listener rather than the admin port.
Prometheus and Loki's backend services remain internal. The gateway authenticates
writes; it does not provide tenant authorization for queries. Keep the administrative
API behind its authenticated private boundary. TLS/public exposure still needs the
routing configuration described above.

### Reconciliation and upgrades

Management server startup installs its supporting components. Linking a target saves
its registration, then queues a separate server-owned reconciliation run. `cluster
reconcile` also queues setup and returns the active run if one is already underway.
The server first saves the live component diff, then installs missing or failed releases
with up to three independent charts running concurrently. Existing managed versions and values require an explicit
upgrade. Conflicts with components owned by another installation block setup. Each Helm
release waits for readiness before its dependants proceed; CRD updates are included during
explicit upgrades. Target run IDs and status survive server restarts. Failed runs remain
inspectable and expose `nextRetryAt`; automatic retries back off from 15 seconds to five
minutes. An explicit reconciliation can request a retry sooner. Each concurrent chart has
its own Helm client caches, and component progress is saved in stable catalog order.
An installation failure blocks its dependants while independent components finish;
the complete reconciliation then reports failure. Explicit upgrades retain their serial order.
Server logs include `component_setup_started` and `component_setup_finished` for each
component, with its scope, chart version, outcome, and `duration` / `duration_ms`.
Component durations include credential preparation, Helm installation, and readiness;
`wait_duration_ms` separately reports time before a component starts, including
dependency and installer-slot waits.
Blocked components report zero active duration. `changed=false` identifies existing
releases that needed no update. `components_setup_finished` reports the total batch
time, including waiting, for management or a specific target cluster.
After a crash, an interrupted Borq-owned pending Helm revision is retained as failed
history before the resumed run creates a new revision. Other pending operations are
left for their owner to finish or recover.

| Scope | Install dependencies | Upgrade order |
| --- | --- | --- |
| Management | OpenBao, ESO and Prometheus can start together. RustFS (dev) waits for OpenBao and ESO; development Loki waits for RustFS and its bucket. Production Loki is independent. | ESO → OpenBao → RustFS (dev) → Loki → Prometheus |
| Target | cert-manager, ESO and Traefik can start together. Alloy waits for ESO and its telemetry Secret. | cert-manager → ESO → Alloy → Traefik |

```sh
borq cluster link local --organization example --kubeconfig ./target.yaml --reconcile=false
borq cluster diff local --organization example
borq cluster reconcile local --organization example --wait=false
borq cluster components local --organization example
borq cluster diff local --organization example --upgrade
borq cluster upgrade local --organization example --timeout 30m
borq management upgrade --api-url http://127.0.0.1:7833
```

The management CLI upgrade calls the running Borq API; it does not install charts
from the CLI host. The API exposes `GET /api/v1/management/components` and
`POST /api/v1/management/components/upgrade`. Registered targets expose `GET /components`,
`GET /components/diff?upgrade=false`, `POST /components/reconcile`, and
`POST /components/upgrade` under `/api/v1/organizations/{organization}/clusters/{cluster}`.
The diff endpoint reads installed chart versions and values without scheduling work.
Target POSTs return the durable queued run immediately; GET components reports its ID,
diff, result, and status (`queued`, `diffing`, `installing`, `ready`, `failed`, or `disabled`).
`cluster link` registers with `reconcile: false`, prints the read-only preview on stderr,
then queues setup through the reconcile endpoint and returns immediately. Reconcile and
upgrade commands wait by default, showing the saved diff once on stderr and the final
structured state on stdout; `--wait=false` returns after enqueueing. Their `--timeout`
bounds polling without cancelling server execution. Direct API registration defaults to
queueing setup when `reconcile` is omitted; the override is never stored or returned.
Management upgrade remains synchronous. Component commands do not upgrade Kubernetes. Upgrades stay within the
installation's component mode: changing between `dev` and `prod` requires a separate
installation or explicit data migration. Borq rejects component mode changes before mutating
management components, including when upgrade is requested.

Existing Flux/Gitea installations require a separate migration. Bootstrap detects
legacy resources in `borq`, `borq-system`, and `flux-system` and stops without deleting
controllers, repositories, or volumes. The old Flux definitions remain reference
material; they are not a runtime dependency.

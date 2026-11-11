# Borq application manager

Borq expands partial application configuration into a complete desired configuration,
renders a small embedded Helm chart, and applies the saved Kubernetes manifests.
SQLite stores the configuration, immutable plans, resource comparisons, and rollout history.

Organizations contain **spaces** and **registered clusters**. Linking a cluster creates its initial deployment space: `default` when available,
otherwise `default-<cluster name>`. Space names are unique within an organization. Every space
must be bound to a registered cluster at creation. Borq assigns its namespace and uses
the `borq` certificate issuer automatically. Spaces have independent app state,
rollout history, queues, and named secrets; multiple spaces can share a cluster.
Traefik, ESO, cert-manager, Alloy, and the CSI snapshot controller are shared per
cluster. Namespaces separate resources; cross-namespace network policy is future
work. User permissions restrict Borq operations within each organization.
The `apps.yaml` file contains no organization, cluster, or space information.

## Development and deployment testing

`borq --version`, `borq version`, and `borq server --version` print the image tag
embedded in the binary. Release builds use the same version for the CLI, server,
and image tag. The server also includes `version` in `/api/healthz`, `/api/readyz`,
and `/api/livez` responses and its startup log.

Local builds report `dev` unless you supply the image tag at build time:

```sh
make build IMAGE_TAG=YOUR_IMAGE_TAG
./dist/borq --version
```

Retagging an existing image preserves the embedded build version.

Build the local Apple Silicon macOS bundle with `make build-macos-bundle`. The archive
is `dist/macos/borq-local-dev-arm64.tar.gz`, with checksum
`dist/macos/borq-local-dev-arm64.sha256`. Extract it and run
`./borq --help`. Keep the included `borq-server-linux-arm64.tar` beside the executable;
development bootstrap can import it with `--server-image-archive`. These local builds are not notarized
and require macOS 13 or newer.

Borq assumes a k3s cluster is already running. Install Kubernetes and configure its
networking separately, then bootstrap Borq on it. Managed networking uses
a management Tailscale OAuth client, a Cloudflare token, and the `network` block
in [management networking configuration](docs/networking.md):

```sh
borq management bootstrap
borq management keys
# Save borq-management-recovery.json in secure storage outside the cluster.
# Start the private port-forward printed by the keys command in another terminal.
borq login --api-url http://127.0.0.1:7833 --token-file ./borq-management-recovery.json
# After login and verifying your secure backup, remove the working recovery file.
borq management apply -f management.yaml
```

Bootstrap installs Borq's namespace, RBAC, persistent SQLite volume, Services and
server Deployment, then returns when Borq's private API is ready. The server
independently reconciles its supporting services, including OpenBao, RustFS and
the shared controllers. It does not wait for `management apply` to start them.
Apply waits for supporting services within its timeout, then configures networking,
organizations and spaces through the Helm Go SDK;
no Helm executable is required.

`management keys` reads existing credentials using the kubeconfig and creates
`borq-management-recovery.json` in your current directory with private permissions.
It prints the absolute path, what to keep, and the next setup command. The file
contains the Borq administrator token, OpenBao administrator token and OpenBao's
auto-unseal key. Keep a secure copy outside the cluster and out of Git. Use
`--output-file PATH` to choose another location. Reruns reuse a matching private
file and refuse to overwrite an incomplete or different file. You can run keys
as often as needed. No login is needed to export the keys, and the command does
not generate or rotate them. If setup is still starting, keys waits up to five
minutes by default; `--timeout` changes that limit. A missing installation gives
a bootstrap instruction. A timeout tells you to let bootstrap finish, rerun keys,
and check `kubectl -n borq-mgmt get pods` if setup has stalled.
Managed networking adds a Tailscale operator in `borq-system`, central ExternalDNS
in `borq-mgmt`, and certificate challenge forwarding from each application cluster.
Cloudflare credentials stay in management. Each separate application cluster supplies
its own Tailscale credentials in cluster YAML. Omit `network` for a fresh local
bootstrap; `make dev` uses this local mode. Later omissions preserve installed integrations.
Production components persist their data. Bootstrap reports API, pod and storage
progress while waiting; supporting services can still be installing when it returns. Repeating
bootstrap preserves the cluster and its state volume, and can update the server image.
No installation name is required. Bootstrap uses the selected kubeconfig cluster name,
removing local `kind-` or `k3d-` prefixes. Cloud-provider keys that cannot be used in
a hostname become a stable DNS-safe name with a hash suffix. Existing installation
names and addresses are preserved even if the kubeconfig name changes. The optional
cluster argument or `--cluster-name` overrides naming only for a new installation.
An older teardown that retained settings without a recorded name may require the
previous `--cluster-name` once; bootstrap reports this before changing resources.
Bootstrap leaves organization creation and target linking to explicit later commands.

Bootstrap and key export leave the CLI logged out. `borq login --token-file`
accepts either a raw API token file or the exported recovery file. It validates
and saves only the Borq token; OpenBao credentials stay out of the CLI login.
Omit `--token-file` to enter a token at a hidden prompt. Apply uses the saved
login and does not accept a recovery-file flag. Once OIDC is configured, use
organization sign-in and account API tokens for everyday access.

Both bootstrap and keys use the current Kubernetes context when `--kubeconfig`
is omitted. They merge `KUBECONFIG` files like kubectl, or use `~/.kube/config`.
An explicit `--kubeconfig PATH` overrides the configured `BORQ_KUBECONFIG` and
current-context selection. To use the local k3s credentials directly, supply
`--kubeconfig /etc/rancher/k3s/k3s.yaml`. Bootstrap connects through the Kubernetes
API and does not require a mapped host port.
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

Development automatically backs up SQLite and OpenBao to the management RustFS
every hour, retaining 24 backups per component. Off-site backups default to disabled.
Existing explicit backup policies are preserved. Encryption keys are managed by
OpenBao. Run `borq management keys` using the development cluster's kubeconfig
and keep the combined recovery file outside the cluster to retain recovery
access. `make dev-kill` removes the disposable environment and its local RustFS
backups.

Development charts install a namespace-scoped pod history job through the External
Secrets chart. Every five minutes it keeps the five most recent failed pod records
per Helm release, shared across that release's components and chart versions. It
uses controller ownership to identify releases and leaves running, successful, and
unowned pod records alone. Production charts do not install this job. `make test-unit`
also runs its retention tests and requires Python 3.

Mirrord steals API and telemetry traffic from the Borq pod into local ports 27833
and 27834. Requests to `localhost:7833` reach the local server. There is no registry
or image rebuild for each edit: Air rebuilds and restarts the server automatically.
Failed builds reuse the last successful binary and the same local database.
UI builds publish only complete output, and the Make targets use a shared lock
to prevent UI rebuilds from replacing files while Go embeds them. This includes
tests running alongside `make dev`. Development and build targets require Python 3.
Go test commands hold this lock until they finish, so long acceptance suites delay
hot reload while the previous version keeps serving.
`make logs` follows the local Borq session output alongside Stern's logs from only
the Borq server container in the `borq-mgmt` namespace, including new and restarted Borq
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
Use the CLI from another terminal (`borq` below denotes a built binary).
First log in with the token file printed by bootstrap. For a standalone server,
use `admin-token` in its `--data-dir` directory.

```sh
borq org create example --file organization-login.yaml
borq org list
borq cluster link local --organization example --kubeconfig /path/to/k3s.yaml
# Use the generated name printed by link (local-a1b2c in this example).
borq cluster reconcile local-a1b2c --organization example
borq cluster list --organization example
borq cluster kubeconfig local-a1b2c --organization example --output ./local.kubeconfig
borq space list --organization example
borq plan -f apps.yaml --organization example --space default
borq apply -f apps.yaml --organization example --space default --wait
```

`organization-login.yaml` supplies the organization's provider and initial
administrator. See [organization OIDC setup](#organization-oidc-setup).
`borq org create NAME` uses administrator-token access until you configure OIDC.
Supplying a provider is optional. Pocket ID can run separately as a managed
application on an application cluster; connect its issuer and client credentials
through the organization's OIDC settings when ready. Existing organization-owned
Pocket ID setup remains available through an explicit configuration or
`--pocketid-organization NAME` for reuse.

`cluster link NAME` requires an organization from flags or client settings. It registers
an existing Kubernetes cluster under `NAME-xxxxx`, with five random hexadecimal
characters. Long name prefixes are shortened to keep generated space names within
63 characters. Borq retries name collisions and creates the cluster and its initial
space together. The first space is `default`; later spaces use `default-<generated
cluster name>`. Existing spaces and bindings are preserved. Use the returned cluster
name for component commands and additional spaces. To inspect or retry an existing
link, use `cluster diff` or `cluster reconcile` with that saved name.
Supply `--kubeconfig PATH`
for the cluster's credentials. `--local OTHER_NAME` remains available for saved legacy
local credentials. Bootstrap does not link its cluster automatically.
The CLI reads and flattens
the kubeconfig, including referenced certificates,
and uploads it to the API. Borq stores the credentials privately with mode 0600;
existing registrations reject different credentials. Organization accounts must
upload self-contained kubeconfigs with inline, static credentials. Only installation
administrators can use exec credential plugins or server-side credential files.
Those administrator kubeconfigs require the plugin and its credentials on the server.
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
metadata. Responses disable caching. This endpoint requires `cluster:credentials`.
`cluster link` first saves the registration, then prints a read-only summary of missing,
changed, and already-installed target components to stderr. `--verbose` includes the
full values diffs; `--output json` or `--output yaml` returns the structured registration
or queued state on stdout. By default it prints a short confirmation and returns after
queueing setup, without waiting for installation. A failed preview leaves the cluster
linked and schedules no new work.
Bootstrap and linking check shared controllers for ownership conflicts. Borq installs
its dedicated `borq-traefik` release in `borq-system`, alongside existing ingress
controllers. Use `--reconcile=false` to preview without queueing setup, or
`--reconcile=false --diff=false` to save only the registration.
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
Traefik, ESO, cert-manager, or snapshot controller releases in other namespaces
are not adopted; migrate those installations before registering the target.
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
use the selected profile or base cluster. An explicit name overrides that selection.
`management bootstrap` instead uses the cluster selected by its Kubernetes kubeconfig,
and preserves the installed management name on reruns. Application commands use the selected space's stored
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
Create the organization and link a cluster to create its initial space. Create additional deployment spaces
with `space create NAME --cluster CLUSTER` using the generated cluster name. Omitting
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

To delete an application, open its **Configuration** tab, expand **Delete
application**, and confirm. The UI shows cleanup progress and returns to the
application list. The API exposes
`DELETE /api/v1/organizations/{organization}/spaces/{space}/apps/{application}`
and MCP exposes `DeleteApp`; both return an operation to poll with `GetOperation`.
Deletion removes runtime resources and retains SQLite volumes, recovery state,
backups, and secrets. The app stays listed until cleanup succeeds so
a failed deletion can be retried. Active deployments must finish before deletion,
and deletion cannot be canceled.

Read an application's deployment history or cancel a deployment using its operation ID:

```sh
borq apps history api
borq apps history api --organization example --space development
borq apps cancel OPERATION_ID
```

History prints the newest twenty deployments as YAML, including operation IDs, plan IDs,
status, timestamps, and each release's application configuration. It uses the selected
organization and space; an empty history prints `[]`. Cancellation needs only the API
selection and operation ID. It returns the current operation as YAML without waiting
for cleanup, so running work may still report `canceling`. Accepted settings and already
applied resources remain in place; completed operations are returned unchanged.

Cluster registrations are scoped to the organization that created them. A physical target
cluster is owned by one registration; registering it again under another organization
or name is rejected before shared controllers or credentials can be changed.

The client also stores the private plan-reference sidecar beside the app file.
Application files and expanded app views reference secrets by name and version;
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
inside the renderer. New spaces, including automatically linked default spaces, use `letsencrypt-cloudflare`
when managed networking is enabled and `borq` otherwise. The issuer is frozen into each plan's render values
and generated Ingress annotations, outside the application spec.

Target installation supplies Traefik's `websecure` entrypoint and cert-manager.
With management networking enabled, Borq configures a `letsencrypt-cloudflare` ClusterIssuer
using cert-manager's built-in Cloudflare DNS01 solver and management-only ExternalDNS.
Cloudflare credentials stay in management. Separate clusters select their existing
certificate issuer per space and supply their own provider credentials; there is no
Borq DNS01 webhook or broker.
See [management networking](docs/networking.md) for bootstrap credentials and domains.
With networking disabled, configure an appropriate ClusterIssuer and DNS separately.
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

## Private container images

In the UI, open the space’s **Secrets** section and choose **Create a secret →
Container registry** to add GitHub credentials. Open the saved registry secret to
edit or delete it. Management credentials are under **Infrastructure → Management cluster
→ Settings → Container registry**. Saved tokens are never displayed.

Registry credentials belong to a space. Spaces on the same application cluster
can use different GHCR accounts and tokens; credentials are projected only into
the configured space's namespace. Management image pulls have their own section.
Registry credentials use the Docker config format understood by Kubernetes for Docker/OCI registries.
The secret name is independent of its registry address, and multiple named credentials can target
the same registry. Existing `ghcr.io` entries keep their names and remain supported.

A management document such as `clusters/cluster.yaml` can configure both scopes:

```yaml
registries: # Management workloads
  ghcr.io:
    username: management-github-user
    token: REPLACE_WITH_MANAGEMENT_TOKEN
organizations:
  - name: example
    spaces:
      - name: production # An existing space, or include its cluster binding
        registries:
          ghcr.io:
            username: application-github-user
            token: REPLACE_WITH_SPACE_TOKEN
```

Apply it with `borq management apply -f clusters/cluster.yaml`. A space file can
contain the same `registries` block alongside `apps`, applied with
`borq apply -f space.yaml --organization example --space production`.
Use a [GitHub personal access token (classic) with `read:packages`](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)
and access to the private package; authorize it for organization SSO when required.

Borq moves inline credentials into OpenBao before saving or queuing the request.
SQLite, saved deployment plans, and management exports contain only a
`credentialsRef`, which can replace `username` and `token` in later applies.
References are bound to their space or management scope and cannot be reused
across scopes. Each space has a namespaced ESO store and a read-only OpenBao
token restricted to that space's registry documents. Remove inline tokens from
your input file after the first apply;
`borq management export` returns the reusable references.

ESO creates immutable `kubernetes.io/dockerconfigjson` Secrets from the pinned
OpenBao documents. Saving credentials creates the space's SecretStore and
ExternalSecret even before it has applications, without waiting for ESO to sync.
Application Deployments, predeploy Jobs, and SQLite jobs receive the space's pull
secrets when deployed; Kubernetes retries image pulls while ESO synchronizes them.
Saving or replacing credentials does not redeploy existing applications. Future
deployments use the saved credentials. Omitted registry settings retain them.
Custom Helm charts must configure their own image pull secret values.

The first management bootstrap needs a public or preloaded Borq image because
Borq starts OpenBao and ESO afterward. Apply management registry authentication
once setup is ready; later bootstrap runs preserve the Borq server's pull secrets.
`make test-registry-auth` checks real OpenBao-to-ESO projection, namespace isolation,
and credential rotation on the existing management development cluster.

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

Apps named in an update run their configured tasks. Omitted apps run tasks only
when a previously accepted change still has an unfinished predeploy barrier. A request without an `apps` field
reconciles the entire space and runs all configured tasks. Supplied predeploy fields
merge into the stored object. Unfinished tasks remain required after cancellation or
failure, including across server restarts. An explicit `predeploy: {command: ""}` disables future executions;
historical Jobs remain for inspection. Disabled settings remain in the expanded config
and in Deployment annotations so manifest readback preserves their execution budgets.
Deployment annotations also retain tasks belonging to apps omitted from an update.
For manifest readback, supply the Jobs from one plan alongside its application resources,
as combining historical Jobs from several plans is ambiguous.

## Secrets

Applications reference separately published secrets. Inline values in an app's
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
The app image version and secret group version are independent.

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
version `"0"`: an empty secret group with no ExternalSecret or generated Secret.
This lets an app with only its name, image, and port deploy without vault setup.
An explicitly named config, or a requested positive version, must resolve successfully;
missing published versions return HTTP 404. Missing store access or credentials fail
planning. Version 0 is reserved for empty configuration and is not an OpenBao version.

Secrets are typed JSON key/value documents stored in OpenBao. Supported types are
`secret-group` for application values and `registry` for private image pulls. The API
keeps the type separate from the data so additional secret types can be added later.

For example, save this as `secret.json`:

```json
{"type":"secret-group","data":{"LOG_LEVEL":"info","OPTIONAL_VALUE":""}}
```

```sh
borq secrets put backend-settings -f secret.json --organization example --space development
borq secrets get backend-settings --organization example --space development
borq secrets get backend-settings --version 1 --organization example --space development
borq secrets list --organization example --space development
borq secrets delete backend-settings --version 2 --organization example --space development
```

Choose a registry secret name such as `github-packages`, independently of its server.
Its document is `{"type":"registry","data":{"server":"ghcr.io","username":"github-user","password":"github-token"}}`.
Registry addresses may include a port or repository prefix; trailing path delimiters are preserved
to retain Kubernetes credential matching. Docker Hub addresses normalize to
`https://index.docker.io/v1/`, its conventional Docker config key. The same credential format
works with Docker v2 and OCI registries; provider-specific access tokens go in `password`.
Passwords/tokens are write-only; reads return the server and username. Omit the password
when editing to keep it; changing the server requires entering it again. The old `token` key
and the `ghcr.io` name without a server remain accepted for compatibility. Saving immediately creates the space's ESO projection. Generated application containers and deployment tasks use `imagePullPolicy: Always`,
including SQLite helpers, so cached images do not bypass the pull request. Applications inherit the
pull secret when deployed; saving, replacing, or deleting credentials never redeploys apps.
Deleting a named registry secret revokes all its saved generations and removes their ESO resources
and Kubernetes pull secrets. Running pods continue running; later image pulls require
new credentials. A reviewed plan using revoked credentials must be reviewed again.

Secret-group publication replaces the complete data map. Omitted keys are removed;
`data: {}` publishes an empty map. Unchanged values reuse the current version. Changed
values create a new OpenBao version, retaining the latest plus ten earlier versions.
An optional `baseVersion` requires the version read by the editor; zero creates only.
Stale writes return HTTP 409. Deleting a secret group destroys all retained values while
preserving its version counter and original vault path. Recreated secrets get a later
version, protecting them from stale editors. Groups referenced by applications and
PostgreSQL-managed secrets cannot be deleted. The old environment-group API/CLI remain
compatible with existing clients and saved application references.

Editing or publishing a config never updates running applications. The next plan using
`latest` selects the new version; explicit version references stay pinned. To prepare
variables for one particular image change, clone a config to a different name and
reference that name in the image's application update. Omitted app fields retain their
previous selectors, so an app that selected `latest` continues selecting latest on
future plans, even though each individual plan saves a concrete version.

With infrastructure management enabled, target registration installs ESO and a
`ClusterSecretStore/borq-openbao` pointing directly to management OpenBao's managed address.
Each target gets its own KV v2 mount and scoped token, stored in `borq-system/borq-openbao`,
key `token`, on that target. Secret group documents are scoped within that mount at
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
- Each space keeps one running apply and at most one queued replacement. A new apply
  stops the unfinished running deployment and supersedes older queued work. Planning
  does not enter the queue. Other spaces deploy independently. Reapplying an already
  accepted plan returns its original operation, including canceled or superseded ones.
- Planning merges changes into the latest accepted desired configuration, so each new
  plan carries earlier accepted updates. An unapplied plan based on an older desired
  revision is rejected with HTTP 409; review a fresh plan before applying it. Borq never
  silently rebases reviewed manifests. Secret group versions remain pinned.
- Execution validates ownership and required resources, synchronizes Secrets, runs
  predeploy, and applies the exact saved manifests. Replacement plans retain unfinished
  predeploy barriers from earlier accepted changes. Execution overwrites manual edits
  to Borq-controlled configuration. Kubernetes defaults and injected sidecars remain
  outside that configuration boundary. There is no continuous drift reconciliation.
- Accepted desired configuration remains saved after failure, cancellation, or replacement;
  the last successful revision is recorded separately. Active and queued work survives
  restart. Older FIFO backlogs collapse to their newest accepted deployment on recovery.
- Use `borq apps cancel OPERATION_ID`, **Cancel deployment** in deployment progress or history, or
  `POST /api/v1/operations/{operation}/cancel` (MCP: `CancelOperation`). Running work
  reports `canceling` while requests stop and unfinished predeploy Jobs are suspended,
  then `canceled` or `superseded`. Queued work can be canceled immediately. Cancellation
  retains accepted settings and resources already applied; it does not undo migrations.
- The server's rollout deadline starts when execution begins, covering Secret synchronization,
  predeploy, and application readiness. The API's `startedAt` field records acceptance time,
  so it can precede execution. `--wait` polls through queued and active states and returns
  a failing exit code on rollout failure, cancellation, replacement, or local timeout; client disconnection does not
  cancel server work.
- Deployment success requires the applied template's replicas to be available and previous
  ReplicaSets scaled down. Managed Ingress success also requires the matching Certificate
  revision to be Ready and a matching, unexpired TLS certificate and private key covering
  the route host. Public DNS propagation and external reachability are not rollout gates.

Deletion commands, pruning, and rollback are deferred. A plan that would remove an
existing Kubernetes resource from its planning baseline is rejected, including clearing
an app list or ingress list that would remove resources. Historical predeploy Jobs and
Secret snapshots are retained. Changing a config's values happens through config
publication rather than an application update. Application pruning, builds, and autoscaling
remain deferred. Custom addons have their own explicit uninstall operation.

See [Manage PostgreSQL addons](docs/managed-addons.md) for managed databases, shared
S3 configuration, automatic backups, and recovery.

## Custom addons

Custom addons belong to a space and use its fixed namespace. Install and manage them from
**Custom addons** in the web UI, `borq addons custom`, the JSON API, or the corresponding MCP tools.
Public HTTP(S) chart repositories and OCI repository prefixes are supported.
In the UI, enter an HTTP(S) repository URL, choose a published chart and version,
then enter the custom addon name and any values overrides. Chart lists are browser helpers;
the API and CLI continue accepting explicit chart references. OCI repositories and
unavailable indexes allow manual chart and version entry.

Deployed addons open on **Overview**, with release details and installation, upgrade,
and Kubernetes events. **Configuration** holds the chart settings and overrides.
**Observability** has **Metrics** and **Logs**, using the same charts, timeline markers,
filters, auto-refresh, and log downloads as applications. Queries select only the chosen
addon in its space. Kubernetes events are matched against resource names from its owned
Helm history, including older retained revisions; unavailable history is reported separately.
`GetCustomChartEvents` returns this timeline independently of pod log collection.

```sh
# Print the selected chart's original values.yaml, including comments.
borq addons custom defaults --repo https://charts.example.com --chart redis --version 1.2.3

# Install using chart defaults plus the complete overrides in values.yaml.
borq addons custom install cache --organization team --space dev \
  --repo https://charts.example.com --chart redis --version 1.2.3 --values values.yaml

# OCI repositories use the same fields.
borq addons custom upgrade cache --organization team --space dev \
  --repo oci://registry.example.com/charts --chart redis --version 1.2.4 --values values.yaml

borq addons custom list --organization team --space dev
borq addons custom get cache --organization team --space dev
borq addons custom logs cache --organization team --space dev --range 12h --search error
borq addons custom metrics cache --organization team --space dev --range 1h
borq addons custom delete cache --organization team --space dev
```

An exact chart version is required. Helm merges overrides with that chart's defaults;
saving a new configuration replaces the complete override map. The UI's **Show defaults** popout shows the original YAML for reference without installing anything or replacing
unsaved overrides. Private repository credentials are outside this initial version.

Installing a chart rejects an existing name in the same space and leaves its settings unchanged.
To update it, open the existing chart and choose **Save and upgrade**, or run
`borq addons custom apply` (`upgrade` is an alias). API and MCP clients must explicitly
set the `apply=true` query parameter to update an existing chart.

Install, upgrade, and uninstall are durable background operations. CLI changes wait by
default; use `--wait=false` to return immediately, then `borq addons custom operation ID` to inspect
progress. API clients poll `GetCustomChartOperation` with the returned operation ID. Updates to
a custom addon with an active operation return a conflict. Interrupted operations resume after
server restart. Values can contain secrets and responses use `Cache-Control: no-store`.

Borq labels chart resources and native Pod templates, including Helm hooks, to identify
the custom addon and space. Custom addon stdout/stderr and CPU/memory are collected for selected pods.
Use `--logs=false` to disable logs and `--pod-selector role=api` to narrow collection to
matching Pod templates. Both controls are also available in the UI and API. Managed
platform components, including Traefik, receive the same collection labels. The collector
keeps only the selected pods' CPU and working-set memory samples before remote write.

Application metrics endpoints are discovered through a Pod port named `metrics` **or**
a Service with all three annotations:

```yaml
metadata:
  annotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "9100" # Must match a port exposed by this Service.
    prometheus.io/path: /metrics
```

Prometheus's broad default discovery jobs are disabled; Alloy owns collection. Pod logs
require `borq.run/logs: "true"`, and CPU/memory retention requires `borq.run/metrics: "true"`.
Kubernetes event retention is limited to Borq space namespaces and the platform namespace.
Bundled CRDs are installed; uninstall retains CRDs. Separating their lifecycle is deferred.
Run `make test-custom-charts` for real Helm lifecycle coverage and `make test-telemetry` for selected
collection against the local management cluster.

## API and persistence

The [OpenAPI contract](api/appapi/openapi.yaml) generates the Go HTTP server and client:

```sh
make generate-api
```

During development, run focused fast checks such as `make test-ui` or
`make test-apps ARGS='-run TestName'`, plus `make fmt` for Go changes.
Run `make pre-commit` before committing: it runs formatting, all unit tests, and
`make test-slow`, which gates the full server cluster suite. Slow cluster tests
are not required after each small edit or at the end of every turn. They require
the running development server and the cluster configured by `DEV_KUBECONFIG_PATH`.

The running server serves the static Astro management site at `/`. Astro builds
the browser assets before Go compilation. The Go binary embeds the complete site,
so production needs no Node.js process, external asset directory, or CDN. Svelte
components load live data from the same server. Select an organization
and space to manage applications, cluster connections, deployment bindings, and
versioned secrets. **Applications** lists apps in the selected space;
select an app to open its own **Overview**, **Configuration**, and **Observability** pages.
**Add an application** opens the same guided editor used for existing apps. Its YAML
checkbox switches between form fields and a single application document, without an
`apps:` wrapper, while preserving unsaved settings. Configuration and saved manifests
show only the selected app. Application changes create a saved plan for review;
applying it queues a deployment whose progress updates automatically. The guided editor
covers the complete application spec: images, replicas, ports, commands, environment
groups and versions, named HTTPS routes, independent readiness and startup paths,
ports, timing and failure thresholds, and predeploy commands with deadlines and retry
limits. Route cards support adding routes and discarding unsaved rows; deployed route
names remain fixed because resource deletion is not supported yet. **Infrastructure**
opens the management cluster, with **Components**, **Application clusters**, and
**Settings** tabs. The sidebar lists application clusters in their own subsection.
Each application cluster has **Components**, **Metrics**, **Spaces**, **Applications**,
and **Settings** tabs. Its Settings tab includes the saved kubeconfig download.
Application and database controllers share the Components tab. Select a component
to inspect its values or customize shared defaults. Management settings contain
backup archive storage and configuration export.

Applications can enable [persistent SQLite with Litestream backups](docs/sqlite-applications.md)
from their editor. Borq exports `SQLITE_DATABASE_PATH=/var/lib/borq/sqlite/app.sqlite`
and stores backups in the management RustFS, including offsite copies when enabled.

**Secrets** lists secret groups and container registry credentials in the selected
space. Choose **Create a secret**, select **Secret group** or **Container registry**,
and enter key/value data. Registry tokens are masked and never returned after saving.
Open an existing secret to edit it, or use its **Settings** tab to delete it. Secret
groups also have **Versions** and **Applications** tabs and can be cloned from Settings.
Applications select published secret groups from a dropdown. Switching between
key/value fields and YAML preserves drafts and publication guards.
An app without public routes shows its internal service address using the space's
server-assigned namespace. The address preview follows the app name and port and
disappears when networking is disabled.

In **Infrastructure**, open **Application clusters** to link an existing cluster using its kubeconfig. Linking queues target
component setup. Create application deployment spaces separately in **Spaces**. Borq manages cluster components
and applications; Kubernetes installation and machine provisioning happen separately.

Installed applications open to **Overview**, with current cluster health, internal and
public addresses, replica scaling, and the latest twenty deployments. Older history entries
offer **New version from this plan**, including failed, canceled, and superseded
deployments. The most recent version does not show this action. This opens its saved application settings in the editor. Edit
them in Form or YAML, then review a fresh plan before deploying. Opening a historical
plan does not create or deploy a new plan. **Configuration** provides Form, YAML, and
Kubernetes Manifests sub-tabs. Form and YAML edit
images, commands, secrets, routes, health checks, and predeploy tasks. Clearing
the predeploy command disables future runs. The **Kubernetes Manifests** sub-tab shows the saved resources.
Reviewing changes replaces the section content with the saved diff; Cancel returns to the
originating section and sub-tab. **Restart** immediately starts a Kubernetes rolling restart
without rerunning predeploy tasks. Reusing settings does not undo database migrations or
persistent data changes. Historical environment versions can be edited before review
and must still be deployable when the new plan is created.

**Observability** has **Logs** and **Metrics** sub-tabs. Logs combines persisted stdout/stderr
from replicas and predeploy tasks, identifying each pod, container, runtime revision, image,
and Kubernetes rollout when these labels were collected. Filter by literal text and time
range, pause automatic refresh, or download the displayed output and events. The view shows
up to 200 newest matching lines; the API accepts up to 1000. Deployment and Kubernetes
lifecycle markers remain visible independently of the text filter. Both charts show the
same events, including observed restart back-offs and repeated container starts. Hover or
use the arrow keys to inspect values, timestamps, and event details; measurement and event
lists also provide accessible text. Empty history and unavailable monitoring have separate
states, and `eventsNotice` reports unavailable or truncated lifecycle history. Monitoring
panels load independently, so an outage does not prevent editing an application.

| History | Chart interval | CPU rate window |
| --- | --- | --- |
| 30 minutes | 30 seconds | 2 minutes |
| 1 hour | 30 seconds | 2 minutes |
| 12 hours | 5 minutes | 5 minutes |
| 3 days | 30 minutes | 30 minutes |

CPU is total cores across replicas (displayed as mCPU); memory is total working-set
bytes (displayed as MiB), averaged over each interval. Missing samples remain gaps,
and accessible measurement tables accompany each chart. History starts when collection
begins; the three-day view does not invent earlier data. Development monitoring retains
up to 96 hours, production seven days, subject to configured storage limits.

The UI sends every action through the same validated API used by the CLI and MCP.
Browser mutations have origin protection, and secret-bearing pages and downloads
are not cached. The page, styles, fonts, and libraries are embedded in the binary.
Run `make test-ui` for frontend compilation, type checks, UI JSON API, validation,
and embedded asset checks. The real deployment, environment publishing, and clone
workflow also runs in `make test-integration-server`. `make dev` rebuilds Astro
and reloads UI changes alongside Go changes.

Frontend development requires Node.js and npm. Run `make build` to build Astro and
produce `dist/borq`, or `make build-ui` to refresh only the embedded site. Release
and macOS builds run that step automatically. Generated `api/webui/site/dist`
assets are checked in so a direct `go build` also produces a complete binary.
Rebuild them whenever frontend sources change.

Run `make test-ui-browser` for Chromium checks against a temporary standalone
server. Install its browser with
`npm exec --prefix api/webui/site -- playwright-core install chromium`, or set
`PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` to an existing Chromium executable.

UI-specific helpers live under `/api/ui/`. `GET /api/ui/page` accepts the browser
selection parameters and returns a component name, JSON view model, and canonical
navigation location. `POST /api/ui/actions` accepts URL-encoded forms and returns
the updated workspace model. Editor conversion, chart discovery, independent
monitoring panels, and ordered log and event timelines use the same namespace.
Domain operations continue through the validated `/api/v1/` API. UI responses and
credential downloads use `no-store`.

The UI uses Basecoat's Vega components with a shared charcoal-and-violet theme.
Colors, typography, radii, and button variants live in [theme.css](api/webui/site/public/assets/theme.css);
buttons, badges, alerts, and empty states use the same component library.
Shared Svelte components in `api/webui/site/src/components` keep the shell
consistent. The appearance switch defaults to dark and saves the
light/dark preference in the browser. Basecoat and Inter are pinned and bundled
locally in the Astro output.

The running server hosts an interactive API explorer at `/api/docs/` and serves its
contract at `/api/openapi.yaml`. The explorer bundles Swagger UI 5.32.15 from its
[upstream release](https://github.com/swagger-api/swagger-ui/releases/tag/v5.32.15)
with Borq, requires no separate service or CDN, and sends requests to the same
server address used to open the page. Application requests
and responses use JSON. The CLI converts `apps.yaml` into the documented `Patch`
request and prints expanded JSON responses as YAML. It rejects duplicate YAML keys
and multiple documents before posting. The API validates unknown fields and explicit
nulls against the same schema. Targets remain in the URL.

The explorer groups reference lookups, configuration previews and conversions, and
aggregate cluster and space dashboard data under `ui-helpers`. These helpers are also
available to CLI and MCP clients; chart defaults, for example, power
`borq addons custom defaults` as well as the UI's **Show defaults** popout.

Domain JSON endpoints live under `/api/v1/`, UI rendering endpoints under `/api/ui/`, the API explorer under `/api/docs/`,
and operational health checks under `/api/`. The old unprefixed API routes are removed.
Keep CLI `--api-url` settings at the server origin (for example,
`http://localhost:7833`); the generated client supplies the API path. Rebuild existing
CLI binaries after this route change. Existing target telemetry configurations need
a component upgrade to adopt the `/api/v1/ingest/` gateway paths.

The server also exposes an MCP endpoint at `http://localhost:7833/api/mcp` using
Streamable HTTP. Tools are derived at startup from the same embedded OpenAPI
contract: each operation ID becomes a tool name, with its summary, description,
and input and output schemas. Each schema includes its referenced definitions so
code mode clients can resolve it independently. The spec also owns `x-mcp-destructive` and
`x-mcp-idempotent` hints: planning is additive, reads are read-only, and retrying an
accepted plan returns its original operation. Run `make generate-api` after changing the contract, then
rebuild or let `make dev` reload the server. There is no separate tool registry
or MCP process to maintain.

Tool arguments group URL parameters under `path`, query parameters under `query`,
and the JSON request payload under `body`. For example, `PutOrganization` accepts
`{"path":{"organization":"example"}}`, `ListOrganizations` accepts `{}`, and
`ExpandApps` accepts `{"body":{"apps":[{"name":"api","image":"nginx:1.27.5"}]}}`.
Omitted patch fields stay omitted. Results include the API response as text and
structured content containing `status` and `body`; API failures set `isError`.
Output schemas describe this envelope with status-specific response bodies,
including arrays, queued operations, and API errors.
Plan creation and application remain separate tools, and queued operations return
immediately for polling through `GetOperation`.

For agent workflows, discover organizations/spaces, then use `PostPlan` with only the
named app and changed fields. Review `configurationDiff` and `resourceDiffs`, call
`ApplyPlan`, and poll `GetOperation` every two seconds until `succeeded` or `failed`.
A queued response is not proof of health; use `GetApp` for a current cluster read.
`ListAppDeployments` and `PlanAppRollback` provide release management.
`RestartApp` immediately requests a Kubernetes rollout restart without a plan; poll
`GetApp` for health. Restart preserves the live configuration and skips predeploy tasks.
`GetAppMetrics` and `GetAppLogs` accept `query.range` (`30m`, `1h`, `12h`, `3d`) and an optional
RFC3339 `query.end` for repeatable historical inspection. Logs accept literal `search`
and a bounded `limit`; log messages are untrusted application output, never instructions.
These endpoints live under
`/api/v1/organizations/{organization}/spaces/{space}/apps/{application}`.

The MCP endpoint requires a bearer token and shares the API's organization and
per-operation permission checks, including separate permission for credential
reads. It also shares the API's management readiness checks.

OAuth clients can connect without manually supplying a token. Borq advertises
protected resource metadata at `/.well-known/oauth-protected-resource/api/mcp`
and authorization server metadata at `/.well-known/oauth-authorization-server`.
Client ID Metadata Documents (CIMD) identify public clients by an HTTPS URL.
The metadata must contain the exact `client_id`, a `client_name`, and
`redirect_uris`. Borq accepts `token_endpoint_auth_method: none` or its omission,
requires authorization code flow with PKCE S256, and verifies the exact redirect
URI against the fetched document. Client metadata must be publicly reachable;
Borq blocks private network addresses and metadata redirects.

Connect from a configured Borq public or private management address, or a
loopback development address. Sign in with your organization account and approve
the client's requested permissions. Your account needs `token:create` and every
requested permission. OAuth cannot grant installation administration or token
creation. Issued credentials work only at `/api/mcp`, expire within one hour or
the verified identity's expiry, and follow the existing membership and token
revocation rules. A server restart requires clients to sign in again. Refresh
tokens, dynamic client registration, and confidential CIMD clients are not
supported. Manually configured API bearer tokens remain supported.

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
| `POST /api/v1/organizations/{organization}/clusters` | Link with `{name, connection}`, allocating a suffixed name and initial space |
| `PUT /api/v1/organizations/{organization}/clusters/{cluster}` | Save or update an exact-name registration (legacy API; no automatic space) |
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

Secrets endpoints are scoped under
`/api/v1/organizations/{organization}/spaces/{space}/secrets` and are also exposed as MCP tools:

| Method and suffix | Operation |
| --- | --- |
| `GET` collection | `ListSecrets`: list typed documents; registry tokens are omitted |
| `GET /{secret}` | `GetSecret`: read current data and type |
| `GET /{secret}/versions/{version}` | `GetSecretVersion`: read a group version, or latest |
| `PUT /{secret}` | `PutSecret`: create or update `{type, data, baseVersion?}` |
| `DELETE /{secret}?version=N` | `DeleteSecret`: delete with an optional current-version guard |

Existing `/environment-groups` endpoints remain compatibility aliases for group
publication, reads, and cloning. App YAML continues to reference groups with `env.name`
and `env.version`; renaming the UI does not change saved application files.

The CLI's YAML round trips preserve configuration values, rather than the original
comments or omitted-field formatting. Inspection handles Borq's Deployment, Service, and
Traefik Ingress shapes; it rejects features that cannot be represented by the app
model. Kubernetes runtime defaults and injected sidecars do not become app fields.
Disabled probe settings use annotations because Kubernetes has no probe fields to hold them.
Secret group names and versions are reconstructed from manifest fields and
checked against the pinned ExternalSecret identity without reading raw vault values.
Include the currently referenced ExternalSecrets alongside Deployments and predeploy
Jobs when inspecting live manifests; historical ExternalSecrets are not part of the
current configuration. Other supported app fields
are read directly, so manual changes remain visible.

The API, MCP, CLI, and UI use the same permission checks. The bootstrap admin
token authorizes server administration across organizations. An organization
login authorizes only that organization. User tokens grant only the permissions
selected when they are created, such as `cluster:link`, within that organization.
API and MCP callers provide a bearer token. Browser sessions authorize the UI.
The server listens on loopback by default. Use `server --listen 0.0.0.0` when running
inside a cluster or serving remote clients. Use HTTPS for remote access.
`server --rollout-timeout 10m` sets the server's rollout deadline independently of
the CLI's `apply --timeout` wait deadline.

Use one server process per SQLite database on persistent local storage. A process lock
prevents two servers from resuming the same operations. Database, journal, and lock files
have owner-only permissions. Multiple CLI clients can use the API; applies queue per
space, and database transactions never wait on Kubernetes network calls.

## Sign in and authenticate the CLI

Open `/login/admin` and enter the token from bootstrap for initial setup or
administrator recovery. After the organization's provider is ready, open `/login`
and choose the organization from the dropdown to sign in with Pocket ID or its configured OIDC
provider. Only organizations with configured sign-in are listed; a single option
is selected automatically. The public selector exposes organization names only.
External OIDC supports sign-in through the configured public address and observed
Tailscale management address. Register both callback URLs printed by
`borq management apply` at your provider. Each login returns to the address
where it started.

External OIDC configuration requires an administrator email. The configured
administrator's verified account receives the Administrator role. Other provider-approved accounts appear in Members after their first login
with no assigned roles. An organization administrator assigns roles before they
can access organization resources. Borq pins identities to issuer and subject;
email changes do not transfer membership.

Borq organization access and Pocket ID administration are separate. The
installation's management administrator configures providers and enrolls the
management-designated Pocket ID administrator. Only management administrators
can edit Pocket ID settings in Borq; organization administrators can read safe
provider metadata and manage their organization's permitted resources.

Top-level `adminEmail` selects the organization's approved provider account for
any provider. It grants no Pocket ID administration and creates no provider
account. When reusing an instance, select an existing account. For a new instance,
the Pocket ID management administrator must create an ordinary account if a
different organization administrator is selected. Account, role and application
access synchronization is not implemented. Existing manually assigned Pocket ID
administrator roles remain unchanged.

Browser **Sign out** clears the Borq session first, then ends the identity-provider
session when the provider advertises an OIDC logout endpoint. Register
`https://YOUR_BORQ_ADDRESS/login` as the client's logout callback URL. Managed
Pocket ID clients receive this URL automatically. If the provider is unavailable,
Borq remains signed out; administrator-token sessions sign out locally. Providers
without a logout endpoint also use local sign-out. Borq does not receive
back-channel logout notifications; leave that provider setting empty.

To use the CLI with a saved token file:

```sh
borq login --api-url https://borq.example.com --token-file ./borq-user.token
borq org list
borq logout
```

An explicit `--api-url` on login becomes the selected profile's API address.
Organization, space, and cluster selections stay unchanged. Login validates the
token and saves an owner-only copy under `~/.config/borq/credentials`, respecting
`XDG_CONFIG_HOME`. Credentials belong to the exact server origin. Borq does not
forward them through redirects. Logout removes the saved copy for that origin.
Token authentication requires HTTPS, except for localhost and loopback development addresses.

If login finds no organizations, the CLI suggests `borq org create NAME`.
The organization uses administrator-token access until OIDC is configured.
To supply your own OIDC provider during creation, generate and edit its configuration:

```sh
borq org create --template=oidc > organization.yaml
# Fill in the administrator email, issuer, client credentials, and public Borq URL.
borq org create example --file organization.yaml
```

After an organization login, open `/account`, choose permissions, and download
a user token for CLI or MCP access. Restrict the downloaded file to its owner
with `chmod 600 borq-user.token` before CLI login. The download expires within
one hour and cannot outlive the verified OIDC identity. The token API accepts
lifetimes of up to 24 hours, also limited by that identity's expiry. The bootstrap
admin token remains available for server setup and recovery.

In **Admin → Members**, create reusable organization roles and assign them to
signed-in members. Permissions belong to roles; a member receives the union of
their assigned roles. Manage invitations and provider accounts at the identity
provider. SCIM synchronization is not implemented.

The CLI exposes the same role and membership controls:

```sh
borq org roles list example
borq org roles create example --template > role.yaml
# Set name and permissions; keep revision at zero for a new role.
borq org roles create example --file role.yaml
borq org members list example
borq org members update example MEMBER_ID --file member.yaml
```

The member file contains `roleIds`, `disabled`, and the last-read `revision`.
Use `roleIds: []` to remove all access. Update a role with
`borq org roles update example ROLE_ID --file role.yaml`, using its current
revision. Assigned roles must be removed from members before deletion.

Role edits apply to existing browser sessions. Removing permissions from a role
also removes them from previously issued API tokens; adding them back does not
restore those token scopes. Changing a member's roles revokes that member's
existing API tokens. Disabling a member revokes browser sessions and API tokens.
Existing direct grants are converted to roles while preserving their permissions.
The API and MCP expose the same revision-checked role and member operations.

## Generate configuration templates

Configuration commands accept `--template` to print every writable field with
placeholder values. Templates come from the OpenAPI definitions embedded in the
CLI. Printing them needs no server, credentials, profile, or existing input file.

```sh
borq management bootstrap --template > management.yaml
borq apply --template > apps.yaml
borq cluster link --template > cluster.yaml
borq space create --template > space.yaml
borq secrets put --template > secret.yaml
borq addons custom install --template > chart.yaml
borq am postgres create --template > postgres.yaml
```

The CLI validates each generated template against its API schema. Replace
placeholders with real settings and remove unused or mutually exclusive fields
before applying. Named organization examples select one provider:

```sh
borq org create --template=oidc > organization.yaml
borq org create --template=pocket-id > organization.yaml
```

Use `--template-format json` for JSON. `borq template` lists API request templates;
you can also request any embedded schema by name:

```sh
borq template PostPlan
borq template PutOrganization --example oidc
borq template OrganizationOidcInput --format json
```

Commands that accept these files validate them against the embedded API schema.
Space creation, custom chart installation, PostgreSQL creation, and PostgreSQL
restore accept `--file`; explicit flags override matching fields from that file.
Custom chart `-f` remains the shortcut for `--values`.

## Organization OIDC setup

Create an organization with `borq org create example` and use the admin token
until OIDC is configured. To supply a provider and initial administrator during
creation, `borq org create example --file organization-login.yaml`
accepts this file:

```yaml
adminEmail: owner@example.com
oidc:
  issuerUrl: https://login.example.com
  clientId: borq
  clientSecret: REPLACE_FROM_YOUR_SECRET_STORE
  borqPublicUrl: https://borq.example.com
```

To configure OIDC later, select an organization and open **Admin → Authentication**.
In **OIDC provider**, enter the client credentials, Borq public URL, and initial
administrator email. Register the displayed callback URL with the provider and
sign in with that account. The administrator token remains available.

Separate organization-owned Pocket ID deployments can still be configured through
management YAML and the API. The Authentication page configures the organization's
OIDC connection; it does not deploy or edit a managed Pocket ID instance.

Borq stores generated credentials in management OpenBao, starts Pocket ID
privately, and creates the administrator and exact OIDC callback before
publishing its ingress. Once public discovery succeeds, the OIDC settings below
are populated automatically. Failed setup can be retried using the same instance
and credentials, including after a Borq restart. A changed OIDC configuration
is preserved until an explicit retry accepts the current revision.

Managed clients also receive a logout callback and a Borq launch URL. Existing
installations gain these links after a server upgrade without changing their
client secrets or sign-in configuration. An existing custom launch URL is kept.
Pocket ID's **My Apps** page is a launcher; edit applications under
**Administration → OIDC Clients** using the provisioned administrator account.

The managed instance permanently blocks Pocket ID's public first-user setup
endpoint. Pocket ID process output and its local proxy access logs are disabled
because the pinned version can log one-time enrollment credentials in request
URLs. Pocket ID's database audit history and Kubernetes health checks remain
available. Client IPs currently appear as the local proxy, so IP-based limits
and audit attribution share that address. Preserving original client IPs will
require an explicitly trusted ingress chain.

The management administrator can create a short-lived administrator enrollment
link through the Pocket ID API. Enrollment is unavailable once that administrator
has a passkey.

Pocket ID keeps its SQLite database on its own persistent volume and continuously
backs it up to RustFS in the same management namespace. Each instance has an
isolated `borq-pocketid-<instance-id>` bucket and restricted backup credentials;
the root RustFS credentials never enter its pod. Litestream takes daily snapshots
and retains seven days of database history under the bucket's `sqlite` prefix.
The backup sidecar uses the management RustFS Service directly, independently of
organization space storage and external primary-S3 settings. Existing managed
instances gain this backup configuration without replacing their OIDC settings.

Backup setup validates storage access and starts replication; it does not report
continuous backup health. Inspect the pod's `sqlite-backup` container logs for
replication failures. Restore archives to a separate volume and retain the original
Pocket ID encryption key from management OpenBao. Missing identity volumes are
never silently replaced with an empty database. Uploaded files are not part of
the SQLite archive; retain their volume separately. This local RustFS copy does
not by itself protect against loss of the management cluster.

Managed deployment settings come from environment variables; user, group and
client management remain available in Pocket ID. When setup finishes, the
installation management administrator can enroll the Pocket ID administrator
passkey. Approved organization accounts sign in to Borq at `/login`. Provider
accounts require Borq authorization before they can access an organization.

To use an existing provider instead, fill in **OIDC provider** manually.
Enter its OIDC issuer and Borq's browser-facing address. The page displays a
callback URL specific to that organization and issuer. Register that exact URI
in a confidential web application at the provider, then enter its client ID and
secret in Borq. Use a tenant-specific issuer for Microsoft Entra ID.

**Save and check discovery** validates provider metadata and stores the secret
in management OpenBao. SQLite retains only the settings and a private vault
reference. Leave the secret blank to retain it for the same issuer and client ID;
changing either requires entering a new secret.

**Test saved sign-in settings** redirects to the provider and verifies the
returned identity using authorization code flow, PKCE, nonce, signature, issuer,
audience, and expiry checks. Start from the saved Borq address in the same browser.
Tests expire after five minutes or a server restart. A successful test records
its time and shows the verified account. This test does not create a Borq session
or grant permissions. Use `/login` for normal sign-in and `/login/admin` for setup
or recovery. SCIM synchronization remains separate work. MCP clients use the
OAuth sign-in flow described above.

### Bootstrap and GitOps YAML

The same management document works at bootstrap and for later updates:

```sh
borq management bootstrap --kubeconfig /path/to/k3s.yaml -f management.yaml --timeout 30m
borq management keys --kubeconfig /etc/rancher/k3s/k3s.yaml
# Follow the printed port-forward, login and management apply instructions.
borq management apply -f management.yaml
```

Bootstrap checks YAML structure and field types before installing anything, waits
for management readiness, then submits the document to that new server for provider
and resource validation and waits for completion.
If configuration fails, the installed management cluster remains available;
correct the YAML and use `management apply` to retry. Omitted blocks preserve
existing settings; apply does not delete omitted resources.

External providers use the organization's `oidc` block:

```yaml
organizations:
  - name: example
    oidc:
      adminEmail: owner@example.com
      issuerUrl: https://login.example.com
      clientId: borq
      clientSecret: REPLACE_FROM_YOUR_SECRET_STORE
      borqPublicUrl: https://borq.example.com
```

Bootstrap accepts external OIDC credentials and keeps the administrator token.
It rejects Pocket ID deployment declarations before accessing Kubernetes. Deploy
Pocket ID separately after bootstrap, including as a managed application in an
application space, then register an OIDC client and supply its credentials here.
See the [Google Workspace setup example](examples/google-workspace-oidc/README.md)
for credential placement, callback registration, and the first administrator.

To explicitly provision an organization-owned Pocket ID after bootstrap, use
`management apply` with this declaration:

```yaml
borqPublicUrl: https://borq.example.com
organizations:
  - name: example
    managedPocketId:
      issuerUrl: https://id.example.com
      adminEmail: admin@example.com
```

This separate operation deploys Pocket ID in `borq-mgmt`, creates its administrator
and OIDC client, and saves the generated credentials in OpenBao. Request the
initial passkey link through the installation-admin-only
`POST /api/v1/organizations/example/pocketid/admin-enrollment` endpoint and open
the returned `enrollmentUrl`. Test Borq sign-in under **Admin → Authentication**.

Pocket ID is a native managed addon. It shares Borq's SQLite/Litestream
configuration and sidecar construction with application workloads, and Borq
supplies the persistent volume, service, ingress and certificate resources. It
uses the management namespace and its RustFS storage without requiring an
application space or a separately linked cluster. No Pocket ID Helm chart is
rendered or installed.

The optional `env` binding selects a management secret group owned by the same
organization. Keys are Pocket ID's exact
[environment-variable names](https://pocket-id.org/docs/configuration/environment-variables),
including branding, SMTP, LDAP, passkey and signup settings supported by the
pinned Pocket ID version. Values must be strings, including booleans and numbers.
Borq supplies `UI_CONFIG_DISABLED=true` so environment configuration is effective.
Database location, encryption and provisioning keys, listeners, logging, file
loaders and other managed runtime settings cannot be overridden. LDAP sync and
ordinary signup are enabled only after initial administrator and client setup
has completed.

Management groups live in OpenBao, independently of application-space groups.
Publish them in bootstrap YAML or with `borq management secrets put NAME --org
ORG --file group.json`; `list` and `get` inspect publications. The JSON input is
`{"type":"secret-group","data":{"APP_NAME":"Example Identity"}}`; an optional
`baseVersion` prevents overwriting a concurrent edit. A publication replaces the
complete map, and unchanged values keep their version. YAML `data: {}` clears a
group; omitting `data` selects an existing `version` (default `latest`). Groups
must name an organization declared in the document.

`env.version` also defaults to `latest`. Borq resolves it when setup or apply is
accepted and retains a private immutable snapshot through retries, restarts and
group history expiry. Publishing a new group version does not change a running
provider: reapply the addon to deploy that publication. The direct editor under
**Admin → Authentication** loads the accepted deployment settings and publishes
and applies its complete map when saved. A numeric version pins a publication. Omitting `env` preserves the
existing binding. To clear group-supplied configuration, publish an empty group
and apply that version. Legacy `values.config` still applies unless reset with
`values: {}`. Group history retains the current and ten previous versions;
an installed exact selection remains usable from its retained snapshot.

Existing `values` / `valuesRef` declarations remain supported as native workload
overrides for resources, scheduling, image pull settings, selected pod metadata
and legacy `config` keys. Secret-group variables override legacy configuration
per key. Borq retains control of persistence, routes and workload security;
unsupported overrides fail validation.

Reapplying unchanged settings preserves the client credentials and last successful
sign-in test. Updating configuration on a ready instance rolls out the workload without
recreating its administrator, client or database. Identity addresses and the
initial administrator are fixed after creation; a failed setup can retry with a
corrected certificate issuer. OIDC edits made in Admin after an operation was
accepted are protected against stale overwrite.

Keep inline `clientSecret`, secret-group data and secret-bearing legacy values out of Git; inject them
when your deployment pipeline renders the input file. Borq does not expand shell
environment references in YAML. `management export` emits `clientSecretRef` and
`valuesRef`, plus secret-group name/version references, instead of secrets or raw values. These references can be reapplied
to the same organization's management vault; they are not portable credentials
for a fresh cluster. Omitting `values` preserves overrides; `values: {}` explicitly
resets them to the managed defaults.

The setup API is `GET` / `PUT /api/v1/organizations/{organization}/oidc`.
Inputs are `issuerUrl`, `clientId`, write-only `clientSecret`, `baseUrl`, and
`revision` (zero initially). Reads include the generated `callbackUrl`, secret
presence, and the last successful test time. Use `make test-oidc` for focused
protocol, storage, and browser-flow checks.

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
| Management, built-in storage | RustFS | 1.0.1 | Persistent S3-compatible PostgreSQL archive storage |
| Management | Prometheus | 29.36.2 | Metrics storage and remote-write receiver |
| Management | Loki | 7.2.0 | Log storage |
| Management and target | cert-manager | 1.21.1 | Certificate controller; issuers configured separately |
| Target | External Secrets Operator | 2.5.0 | Materialize app and Alloy secrets from management OpenBao |
| Management and target | Traefik | 42.0.0 | Shared management and application ingress |
| Target | Alloy | 1.12.1 | Collect target logs and metrics |
| Target | CSI snapshot controller | 5.2.0 | Install snapshot APIs and their shared controller |

Management bootstrap creates `borq-mgmt` for Borq. The server creates
`borq-system` and installs OpenBao, storage, telemetry and shared controllers
automatically. Management apply configures the installation. Linking the same cluster reuses those
releases and adds workload ESO, Alloy and the snapshot controller in `borq-system`.
Organization and space registration remain explicit. A converged cluster runs a
separate ESO installation for each role.
Borq installs Helm release and Service `borq-traefik` in `borq-system`, with a
non-default ingress class and explicit provider selection. Borq routes use
`kubernetes.io/ingress.class: borq-traefik` without `spec.ingressClassName` so an
unfiltered K3s Traefik does not also claim them. K3s keeps its existing routes.

The default exposure is `NodePort`, with Kubernetes allocating unused HTTP and
HTTPS ports. Read them with `kubectl -n borq-system get service borq-traefik` and
configure the external proxy or port mappings to reach those ports. Public DNS
alone cannot select a NodePort. A separate load balancer address can instead be
configured through `components.traefik.values.service.spec.type: LoadBalancer`.
The Borq API's Tailscale access remains independent of this public ingress path.

On upgrade, Borq recognizes its legacy `borq-system/traefik` release by the stored
chart provenance, starts the dedicated controller, moves Borq-owned Traefik routes,
and retires the old release. Existing public traffic must be directed to the new
exposure. Resources belonging to other releases, CRDs and persistent volumes are
preserved. A same-named release without Borq provenance stops migration.
Management ESO watches only `borq-mgmt` with namespace-scoped permissions and a local
`SecretStore`. Application ESO uses the `borq-system` controller class and the scoped
`borq-openbao` ClusterSecretStore. ESO CRDs are shared cluster infrastructure outside
either Helm release, so removing one installation preserves the other.
The snapshot controller uses the Piraeus chart recommended in
[k3s issue #2865](https://github.com/k3s-io/k3s/issues/2865). Snapshots also require
a compatible CSI storage driver and a matching `VolumeSnapshotClass`. Borq does
not create driver-specific snapshot classes. The k3s `local-path` provisioner
does not support CSI snapshots.
Managed networking adds ExternalDNS in `borq-mgmt` and a shared Tailscale operator
in `borq-system`, with cert-manager's native Cloudflare DNS01 solver; see [networking setup](docs/networking.md).
Persistent standalone RustFS is
the default managed PostgreSQL archive store in every profile. Production requests
100Gi by default. Explicit external S3 settings remain authoritative and avoid an unused
production RustFS installation. Dev Loki uses RustFS, while production Loki keeps its
existing filesystem selection unless external S3 is explicitly configured. RustFS uses
a Recreate deployment strategy so a single data volume has one writer during upgrades.
The **Backup archive storage (S3)** panel configures the primary archive store.
Scheduled native SQLite and OpenBao captures use that store. Restic offsite backups
copy completed management backups and PostgreSQL archives to a separate external S3
repository. Configure local captures with `backups` and offsite exports with `offsite`
in management configuration. See [management and offsite backups](docs/offsite-backups.md)
for setup and independent recovery.

The baseline uses single replicas, small local volumes, and no HA. Borq's SQLite
volume is 1Gi. The `borq-local` StorageClass uses k3s's local-path provisioner, waits
for a consuming pod, and retains volumes. Dev Prometheus and Loki each use 1Gi with
24-hour retention; production defaults use 5Gi with seven-day retention. Supply
`--storage-class` when an existing cluster uses another provisioner.

### Production and existing clusters

Start with an existing k3s cluster and its kubeconfig:

```sh
borq management bootstrap --kubeconfig /etc/rancher/k3s/k3s.yaml
borq management keys --kubeconfig /etc/rancher/k3s/k3s.yaml
# Follow the printed port-forward, login and management apply instructions.
borq org create example --file organization-login.yaml
borq cluster link management --organization example --kubeconfig /etc/rancher/k3s/k3s.yaml
```

Kubernetes installation, machine access and networking are handled outside Borq.
Management bootstrap installs Borq, which reconciles supporting components.
Management keys saves the recovery file; management apply configures the installation. Linking
an existing cluster enrolls it as a deployment target. Create application deployment spaces separately.

Both profiles use persistent, single-node OpenBao Raft. Borq saves OpenBao's
administrator token and static auto-unseal key in `borq-mgmt/borq-openbao-admin`
before starting OpenBao. OpenBao self-initializes using those saved credentials.
Borq restarts can resume without losing a one-time initialization response.
OpenBao crashing during its own initialization can still require manual repair;
Borq never wipes its volume or replaces missing recovery material automatically. This is an initial local-disk baseline, not a production HA
or independently secured recovery design; protect Kubernetes access and back up both
vault data and independently retained recovery material. Native management backup encryption is managed automatically through OpenBao. Its
private key is derived from the auto-unseal key already exported by `borq management keys`;
there is no separate backup key file to generate or retain. Automated static-seal
OpenBao recovery remains deferred to `management bootstrap-from-restore`. SQLite restoration
pauses queued operations until explicit offline review, storage rebinding and resume.

A standalone server enables its own component setup using `--management-enabled` and
target installation using `--infrastructure-enabled`, together with
`--management-kubeconfig` (omit inside the management cluster),
`--api-url` with an advertised management address, `--infrastructure-profile`,
and `--infrastructure-storage-class`. Reconciliation derives component addresses
from that management host. The bootstrapped server already receives these settings.
Its HTTP listeners start before management setup. `/api/readyz` and `/api/livez`
check the API's local SQLite connection independently of component setup and network
availability. `/api/healthz` includes management setup status and remains unavailable
until management components and configured networking are ready. Bootstrap waits on
`/api/readyz`; operations that need management components remain gated while
the API stays accessible for diagnosis and recovery.
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

New application and predeploy pods carry `borq.run/logs=true` and
`borq.run/metrics=true` with stable app, space, organization, and cluster identity
labels. Existing Borq-managed app pods remain collected while those flags are adopted.
Alloy discovers log-enabled pods and stores `borq_app`, `borq_space`,
`borq_organization`, `borq_cluster`, namespace, pod, and container labels in Loki.
Runtime revision, image, and pod-template-hash labels preserve version identity across
rollouts. The revision remains stable for scaling and routing changes. Alloy also watches
Kubernetes events and persists them in Loki for historical deployment and restart context;
that lifecycle history begins when the collector configuration is enabled.
For CPU and memory, Alloy scrapes kubelet cAdvisor through the Kubernetes API every
30 seconds with a service-account token and CA verification, then forwards the two
container usage metrics to management Prometheus. Queries select the exact cluster,
namespace, app container, and deployment pods, excluding predeploy task usage. No
application `/metrics` endpoint or sidecar is required.

Alloy reads telemetry credentials from a mounted Secret file so ESO credential refreshes
can be picked up without retaining an old environment value. On an existing installation,
run `borq cluster upgrade NAME --organization ORG` to adopt the collector configuration;
existing apps receive the new pod labels on their next reviewed deployment. Management
component upgrades adopt the longer development retention. Private queries default to
the selected management cluster’s authenticated Service proxy. `BORQ_PROMETHEUS_QUERY_URL` / `--prometheus-query-url` and
`BORQ_LOKI_QUERY_URL` / `--loki-query-url` can select private query stores when running
outside the cluster; ingestion URLs remain separate.

A key authorizes only its matching cluster route. Borq stores only the telemetry
key's digest in SQLite; raw credentials stay in OpenBao and target Secrets.
The public `borq-telemetry` Service selects the separate ingestion-only listener on
port 7834 (NodePort 31834). It has no administrative API routes. Development publishes
this listener on host port 7834 while the administrative API's host port stays on
loopback. Point public telemetry routing at this listener rather than the admin port.
Prometheus and Loki's backend services remain internal. Borq reaches both stores through
the selected management cluster’s authenticated Service proxy, including during local
development. The gateway authenticates writes. Administrative queries use Borq
authentication and organization permissions. HTTPS access still needs the routing
configuration described above.

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
left for their owner to finish or recover. Startup can recover shared cert-manager
and Traefik operations recorded as interrupted by either management or the converged
target, before target workers resume.

| Scope | Install dependencies | Upgrade order |
| --- | --- | --- |
| Management | OpenBao, ESO, Prometheus, cert-manager and Traefik can start independently within the three-install limit. RustFS waits for OpenBao and ESO; development Loki waits for RustFS and its bucket. Production Loki is independent. | ESO → OpenBao → RustFS → Loki → Prometheus → cert-manager → Traefik |
| Target | cert-manager, ESO, Traefik, and the snapshot controller can start together. Alloy waits for ESO and its telemetry Secret. | cert-manager → ESO → Alloy → Traefik → snapshot controller |

```sh
borq cluster link local --organization example --kubeconfig ./target.yaml --reconcile=false
# Replace local-a1b2c below with the generated name printed above.
borq cluster diff local-a1b2c --organization example
borq cluster reconcile local-a1b2c --organization example --wait=false
borq cluster components local-a1b2c --organization example
borq cluster diff local-a1b2c --organization example --upgrade
borq cluster upgrade local-a1b2c --organization example --timeout 30m
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

## Management public exposure

Management networking has two explicit DNS paths: `network.cloudflare.exposure:
tunnel` publishes the management tunnel's CNAME, while `exposure: direct` publishes
Traefik's public Gateway address or the optional static IP/load-balancer hostname
in `directTarget`. ExternalDNS owns the records and Traefik owns backend routing.
A pending tunnel preserves its current record instead of switching to a public
load balancer. Existing `tunnel.enabled` configuration remains supported. See
[management networking](docs/networking.md#exposure-modes) for full examples and
TLS requirements. These new exposure fields require a CLI and server containing
the change; they are not part of the published rc6 schema.

## Cloudflare Tunnel for management ingress

Management bootstrap can run an outbound `cloudflared` connector for managed
Pocket ID hostnames. Merge this block into the existing management YAML; retain
its domain, zone ID, ACME email and Tailscale settings:

```yaml
network:
  cloudflare:
    wildcardDomain: prod-test.borq.io
    zoneId: YOUR_EXISTING_ZONE_ID
    acmeEmail: YOUR_EXISTING_ACME_EMAIL
    tunnel:
      enabled: true
```

Use the existing `BORQ_CLOUDFLARE_API_TOKEN`. Add **Account → Cloudflare Tunnel →
Edit**, restricted to the account containing the zone. Retain **Zone → DNS → Edit**
and **Zone → Zone → Read**, restricted to that zone. No account-read permission or
separate user-supplied tunnel token is required: Borq derives the account ID from
the configured zone and retrieves a tunnel-specific run token. The connector pod
receives only that run token through a Kubernetes Secret file, never the account
API credential. Re-bootstrap with the updated token to rotate the saved credential.
Do not place API tokens in the YAML or commit them to source control.

This is a bootstrap networking option: rerun `borq management bootstrap` with the
existing installation name, kubeconfig and complete management file when enabling
it on an existing installation. `management apply` rejects changes to the runtime
bootstrap network settings and tells you to re-bootstrap. The running server and
CLI must include this feature; rc3 does not support this block.

Borq provisions a stable installation-specific remotely managed tunnel, configures
`*.<wildcardDomain>` to reach the private `borq-traefik` HTTPS Service, and runs
two pinned `cloudflare/cloudflared` replicas. The origin Host/SNI follows the
incoming public hostname; origin certificate verification remains enabled. The
connector requires outbound connectivity to Cloudflare, including tunnel traffic
on port 7844. Readiness uses cloudflared's `/ready` endpoint and therefore requires
an actual connection; a running pod alone is not reported as ready.

ExternalDNS remains the DNS owner. Borq generates proxied CNAME `DNSEndpoint`
resources pointing accepted, live, Borq-owned Pocket ID HTTPRoutes to
`<tunnel-id>.cfargotunnel.com`, even when the Gateway has no load-balancer address.
Routes on other deployment clusters retain their existing load-balancer DNS
behavior; this option does not redirect them through management. Account API
credentials and connector tokens remain in management, not on target clusters.
A successful connector rollout does not prove public DNS propagation or edge TLS;
Pocket ID setup still validates public OIDC discovery before completing.

Cloudflare must have an active edge certificate for the chosen hostname. On a
full-setup zone, standard Universal SSL covers the zone apex and one subdomain
level; a deep hostname such as `id.prod-test.borq.io` needs additional
edge certificate coverage or an appropriate separately configured zone. The
cert-manager origin certificate does not supply Cloudflare's browser-facing edge
certificate. Tunnel and DNS edit permissions do not authorize certificate purchases.

Set `tunnel.enabled: false` and re-bootstrap to scale the owned connector to zero.
The remote tunnel and DNS records are retained for recovery; disabling the tunnel
does not automatically restore public reachability through NodePorts. Omission
preserves the existing integration when the entire Cloudflare block is omitted.
No unrelated tunnel, DNS record, or Kubernetes resource is removed.

The `network.cloudflare.wildcardDomain` setting accepts either `*.prod-test.borq.io` or `prod-test.borq.io`; both normalize to the same scope. The tunnel wildcard is used directly, without adding a `management.` prefix. Managed identity hosts occupy one name below that suffix (for example, `id.prod-test.borq.io`). Target applications retain their separate `<cluster-id>` subtree.

### Detach a management installation

Sign in with the installation administrator token, then print a read-only
plan using explicit Kubernetes credentials. `--api-url` selects the saved login;
requests use the private Kubernetes Borq Service proxy so public routes can be
removed without losing the management connection.

```sh
borq login --api-url https://borq.example.com --token-file ./borq-admin-token
borq management destroy --api-url https://borq.example.com --kubeconfig ./management.kubeconfig --namespace borq-mgmt
```

Review the owned resources and retained items, then add `--confirm`:

```sh
borq management destroy --api-url https://borq.example.com --kubeconfig ./management.kubeconfig --namespace borq-mgmt --confirm
```

The server queues durable cleanup, pauses ordinary management changes, and stops
managed Pocket ID workloads before removing their routes, gateways and exposed
Services. It verifies Cloudflare DNS against this physical cluster's ExternalDNS
TXT ownership receipts and existing routing targets, removes data records before
their receipts, and retires the exact stored Cloudflare tunnel. ExternalDNS
pauses during direct cleanup and resumes after its owned DNS sources are gone.
Competing ownership receipts or changed targets block deletion. The tunnel
receipt is removed only after verified remote deletion.

Borq keeps its private API, Kubernetes authority and core resources until server
cleanup has completed and an immutable completion receipt is stored in
Kubernetes. The CLI checks that receipt against the selected physical cluster,
namespace, operation and original resource identities, then removes only Borq's
remaining core resources and bootstrap binding. The CLI performs no provider
cleanup and never reads the administrator token from a Kubernetes Secret.

Kubernetes finalizers must finish before server cleanup completes. The Tailscale
operator remains available to remove its proxies and devices. The server also
removes its management admin egress Service from `borq-system` and waits for the
operator to remove the corresponding proxy Pods, StatefulSet, headless Service,
private proxy state and tailnet device. It retains the shared operator and other
installations' proxies. Teardown never
strips finalizers or claims unavailable providers were cleaned up. `--timeout`
bounds the local CLI wait; an accepted server operation continues if the CLI is
interrupted. Keep credentials and receipts, resolve any reported failure, and
retry the same command. A completed receipt also permits safe finalization after
Borq stops; replacement installations are never removed using an old receipt.
An unavailable server without a valid completion receipt requires server
recovery before teardown can proceed.

This server-coordinated workflow is a source correction after `0.0.1-rc10`.
The published RC10 binary uses the earlier CLI-owned teardown implementation.

This detaches Borq; it does not uninstall Kubernetes or shared infrastructure.
The namespace, PVCs/PVs, database and backup data, Borq credentials/admin tokens, recovery
configuration, shared Helm controllers and certificate lifecycle remain.
Cert-manager retains Certificates, Issuers and ACME challenges, including their
DNS01 cleanup. Shared Traefik load balancers can remain because they may serve
other workloads. The exact bootstrap ClusterRoleBinding and Borq ServiceAccount
are removed to revoke the detached server's Kubernetes authority; shared roles
and unrelated resources remain. These retained resources can be reused during
bootstrap and must be reviewed separately before deleting the cluster itself.
After bootstrap restarts Borq, it rebuilds managed Pocket ID workloads and routes
using their retained storage and credentials. A ready provider must retain its
owned PVC and original credential Secret; Borq verifies its existing OIDC client
and secret before publishing the recovered provider. Missing identity data blocks
recovery rather than creating a replacement account or resetting OIDC settings.

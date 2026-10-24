# Manage PostgreSQL addons

Managed PostgreSQL gives each database its own PostgreSQL cluster in Kubernetes, automatic S3
backups, continuous WAL archiving, and scheduled recovery checks. Use an existing
Borq organization and space bound to a registered deployment cluster.

## Configure shared storage and organization access

Borq uses one S3 connection for management storage and managed addons. In
development, Borq prefills the existing RustFS connection. In production, configure
an external S3-compatible endpoint and an existing bucket. Set `managedAddons: true`
on each organization that may use the shared connection.

Start from a safe export of the current configuration:

```sh
borq management export > management.yaml
```

Edit `management.yaml` using the following structure. Replace the example endpoint,
bucket, credentials, cluster connection, and application image with your settings.
The kubeconfig path is on the Borq server. An inline `kubeconfig` string is also
accepted for a new cluster registration.

```yaml
components:
  cloudnative-pg:
    repo: https://cloudnative-pg.github.io/charts
    chart: cloudnative-pg
    version: 0.29.1
    values: {}
  plugin-barman-cloud:
    repo: https://cloudnative-pg.github.io/charts
    chart: plugin-barman-cloud
    version: 0.8.1
    values: {}
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: borq
  prefix: addon-archives
  pathStyle: true
  accessKey: REPLACE_ME
  secretKey: REPLACE_ME
organizations:
  - name: example
    managedAddons: true
    clusters:
      - name: production
        kubeconfigPath: /path/on-borq-server/target.yaml
    spaces:
      - name: default
        cluster: production
        apps:
          - name: api
            image: nginx:1.27.5
        customAddons: []
        managedAddons:
          postgres:
            - name: database
```

There is no top-level `version` or `profile`. Components are a map keyed by Borq
component name. Their `values` maps contain inline Helm overrides. Omit `components`
to keep the configured component settings.

Supply `accessKey` and `secretKey` together, with an optional `sessionToken`, or keep
the `credentialsRef` from an export. Do not combine inline keys with a reference.
Borq validates storage access and moves inline keys into OpenBao before accepting
queued work. Exports contain references rather than keys. Keep any input file that
contains inline credentials private.

The management bucket may also hold addon backups after the explicit organization
grant. Borq isolates archives by stable organization and database identities under
`s3.prefix`. This prefix applies to managed-addon archives. It does not move or
isolate Loki objects.

Apply the document and export the accepted configuration:

```sh
borq management apply -f management.yaml
borq management export > management.export.yaml
```

Management apply creates and updates named resources, including requested component
upgrades. It preserves resources omitted from the file, including names omitted
from a list or an empty list. Delete resources explicitly. Storage credential
replacement validates the new credentials first. Borq blocks storage removal or
location changes while management components, live databases, or preserved archives
depend on the connection.

## Create or update a database

Save this space fragment as `database.yaml`:

```yaml
managedAddons:
  postgres:
    - name: database
```

Apply it to the selected organization and space:

```sh
borq am postgres apply -f database.yaml --organization example --space default
borq am postgres get database --organization example --space default
```

The default is one instance with 1Gi storage, the tested PostgreSQL image, and an
application database and role both named `app`. Borq installs its PostgreSQL
controllers on demand. An existing independently installed controller requires
explicit migration before Borq can manage databases on that cluster.

Space fragments use the same `apps`, `customAddons`, and `managedAddons` fields as
each space in `management.yaml`. Custom addon entries have `name`, `repository`,
`chart`, an exact `version`, and optional inline `values`, `logs`, and `podSelector`.
Applications use the existing application patch schema. Apply a shared fragment
with `borq am postgres apply -f FILE`; its named apps and both addon kinds are applied.

For a single database, use the CLI directly:

```sh
borq am postgres create database --organization example --space default
borq am postgres apply database --storage 2Gi --organization example --space default
borq am postgres list --organization example --space default
```

Choose either file apply or direct create for the initial creation. `create` rejects
an existing name. `apply` creates or updates it and preserves omitted settings.
`borq am postgres` is the shortcut for `borq addons managed postgres`.
`borq ac` is the shortcut for `borq addons custom`. Both use the same organization
and space flags.

Create, apply, delete, and restore wait for completion by default. Use `--wait=false`
to return after acceptance, then inspect the returned operation ID:

```sh
borq am postgres operation OPERATION_ID
```

`--timeout` limits the CLI wait. Server work continues after that timeout, and queued
operations resume after a server restart.

## Retrieve connection credentials

Request credentials explicitly through the CLI:

```sh
borq am postgres env database --organization example --space default
borq am postgres env database --organization example --space default --output json
```

The output contains `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD`, and
`DATABASE_URL`. Treat the output as a secret. Database lists, details, and
configuration exports do not include these credentials. The hostname is reachable
from the database's Kubernetes network.

## Connect an application with an environment group

Open the database's **Overview** page and follow the group link under
**Application connection**. Borq creates a managed environment group named
`postgres-<database-name>-<database-id>` with the same six connection variables.
The database ID keeps the group tied to this database for its lifetime.
External Secrets publishes these values to OpenBao. Reconciliation also creates
groups for existing databases automatically.

Open your application's **Configuration** page, select this group in the
**Environment group** selector, then review and deploy the change. Borq refreshes
the group's connection values automatically and publishes a version when they
change. Existing deployments keep their pinned version until you review and
deploy a new plan.

Managed groups are read-only. You can inspect their values and versions, or use
**Settings** to clone a published version into an independent group. Edit and
publish that copy as needed. A clone does not receive automatic database updates.

Deleting a database retains its environment group and credential history. Treat
the saved values as secrets. A restored database, or a database recreated with the
same name, gets its own group. Existing application references keep pointing to
the original group until you change them.

## Inspect protection and restore a backup

Borq starts an initial full S3 backup, then schedules full backups daily at midnight
UTC. Continuous WAL archiving provides a 30-day recovery window by default. The
catalogue reports the recovery bounds currently available for each backup.

CSI snapshots run hourly with 24-hour retention when the cluster supports them.
The default `snapshots: preferred` reports `unsupported` when CSI snapshots are
unavailable. Use `required` to fail provisioning without snapshot support, or
`disabled` to turn off CSI protection. S3 backups and WAL remain mandatory.

List real backup metadata and select an entry with `eligible: true`:

```sh
borq am postgres backup list database --organization example --space default
borq am postgres backup restore database-restored --backup BACKUP_ID --organization example --space default
```

Use the opaque Borq `id` from the catalogue as `BACKUP_ID`. Restore always creates a
new database and rechecks backup availability and compatibility before execution.
For point-in-time recovery, add `--recovery-target` with an RFC3339 timestamp within
the entry's `recoveryStart` and `recoveryEnd` bounds. Retrieve the restored connection
with `borq am postgres env database-restored --organization example --space default`.

Weekly recovery checks run by default on Sunday at 03:00 UTC. Each check restores an
isolated copy, verifies SQL access, and cleans up the copy. Checks do not overlap.
Database details show the result, duration, snapshot-check result, and cleanup state.

Delete a live database and its volumes explicitly:

```sh
borq am postgres delete database --organization example --space default
```

Deletion preserves its backup archive. `backup list` can still discover preserved
backups using the source name. Routine backups are automatic. The backup API exists
for automation and recovery testing; the CLI has no backup-create command.

## Adjust advanced settings

Add optional fields to an entry in `managedAddons.postgres` for `instances`,
`storage`, `storageClass`, `image`, `database`, and `username`. Resource sizing is
optional. Set CPU and memory under `resources.requests` and `resources.limits`.

Use `backup.schedule` and `backup.recoveryWindowDays` to change S3 protection.
Use `snapshotSchedule`, `snapshotRetentionHours`, and `snapshotClass` to change CSI
protection. Schedules use five cron fields in UTC.

Set `restoreCheck.enabled: false` to disable scheduled recovery checks. Otherwise,
adjust `restoreCheck.schedule`, `restoreCheck.timeoutSeconds`, and the optional
`restoreCheck.resources` requests and limits. The default timeout is 1,800 seconds.

In the web UI, configure shared S3 and organization access in **Settings**. Select a
space and open **Managed PostgreSQL**. Creation requires only a name. **Advanced
settings** exposes sizing and configuration. Database details include protection,
restore, settings, CPU, memory, disk, connections, active queries, and logs.

Read metrics and bounded logs from the CLI:

```sh
borq am postgres metrics database --organization example --space default --range 1h
borq am postgres logs database --organization example --space default --range 1h --limit 200 --search error
```

History ranges are `30m`, `1h`, `12h`, and `3d`. Logs accept a literal search and a
limit of 1 through 1,000 lines. Metrics report unavailable observations explicitly.
The API explorer at `/api/docs/` describes the same operations used by the UI, CLI,
and MCP tools.

## Verify provisioning changes

For changes to managed PostgreSQL provisioning or recovery, run the dedicated real
backup-and-restore check against a ready development installation:

```sh
make test-managed-postgres-e2e
```

The target uses `tmp/management/kubeconfig` and the local API at port 7833 by default.
It creates temporary addons, seeds data, backs up, restores, compares schema and
data, checks database writes, and cleans up. Use focused Make checks while iterating.
Run `make pre-commit` outside the sandbox before committing. The dedicated recovery
check is additional coverage for provisioning changes, not a routine UI or copy-edit
check.

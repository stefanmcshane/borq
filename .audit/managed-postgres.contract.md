# Managed Postgres implementation contract

## Ownership decision

Keep `apps.Manager` as the sole SQLite and worker lifetime owner. Add separate managed Postgres records/tasks/archive collections, not application rollout plans or their per-space queue. Concrete Kubernetes and S3 mechanisms may live in foundation packages. No generic provider service or shared-database constructor migration is needed. Borrow the alternative candidate's immutable archive identity and separate action payloads.

Existing application EnvSnapshot persistence is outside this change. New S3/managed task records must contain only credential references. Do not claim this change removes historical application secrets.

## Public document

Use `components` as a map keyed by component name, matching the user example. Each entry accepts `repo`, `chart`, `version`, and inline `values`. No top-level version or profile.

Use `s3` fields `endpoint`, `region`, `bucket`, `prefix`, `pathStyle`, `accessKey`, `secretKey`, optional `sessionToken`, or `credentialsRef` for safe exports. Input keys are transient and persisted in OpenBao before accepting work. One connection serves management and managed addons. Organization `managedAddons: true` explicitly grants use of that connection. Production rejects local RustFS; dev prefills the same connection schema.

Organizations and spaces are named arrays. Organization entries have name, optional managedAddons grant, clusters, and spaces. Space entries have name, cluster, apps, customAddons, and managedAddons. `apps` reuses AppPatch. `customAddons` entries use the existing AddonInput plus name. `managedAddons.postgres` is an array of named PostgresInput. Standalone space files contain those same apps/customAddons/managedAddons fields. Omitting a name or using an empty named list never deletes resources. Explicit deletion remains a separate command.

## Manager surface

Use existing-style methods taking context, organization, space, name plus typed input. This keeps transport adaptation consistent with custom addons.

- `PutPostgres(ctx, organization, space, name, PostgresInput, apply bool) (Postgres, error)`.
- `GetPostgres`, `ListPostgres`, `DeletePostgres` mirror current addon signatures.
- `GetPostgresOperation(ctx, id) (Operation, error)`.
- `GetPostgresEnv(ctx, organization, space, name) (map[string]string, error)` returns PGHOST/PGPORT/PGDATABASE/PGUSER/PGPASSWORD/DATABASE_URL only at this explicit secret endpoint.
- `ListPostgresBackups(ctx, organization, space, name) ([]PostgresBackup, error)` retains deleted sources through archive records. Each catalogue entry has opaque ID, source identity, completion time, major version, size and recovery range/eligibility.
- `RestorePostgres(ctx, organization, space, name, PostgresRestoreInput) (Postgres, error)` reserves a new destination and revalidates the selected source before execution.
- `BackupPostgres` is a production internal/API capability for automation and E2E; no routine CLI backup-create action.
- `GetPostgresMetrics(ctx, TelemetryInput) (PostgresMetrics, error)` and `GetPostgresLogs(ctx, TelemetryInput) (AppLogs, error)` reuse scoped telemetry.
- `GetManagementConfiguration`, `ApplyManagementConfiguration`, and `GetManagementOperation` expose safe export and durable aggregate apply.
- `GetManagementS3`, `PutManagementS3`, `DeleteManagementS3` expose shared storage settings, validated rotation and dependency guard.

Exact supplementary type fields are owned by the relevant writer. Publish them early and update this contract when integration accepts a deviation. No caller may orchestrate external lifecycle stages.

## Writer boundaries and handoffs

1. Configuration writer owns new management/S3 domain files, `foundations/objectstore`, platform chart overrides/shared S3 consumption, organization grant and aggregate apply. It owns any dependency additions. Publish the concrete S3 config/client types and Manager storage resolution seam first, before implementing the rest.
2. Postgres writer owns typed Postgres model, durable lifecycle, archive catalogue/restore, credentials, CNPG/plugin/snapshot manifests, restore-check execution and monitoring. Publish model and public signatures first. It can request additions to platform from configuration writer but does not edit that writer's files. It owns Manager lifecycle hook edits; config writer communicates its hook needs.
3. Transport writer owns OpenAPI, generated code, HTTP handlers, CLI, and web UI, consuming the above names. It is the sole API generator owner. It can start schema and view construction while backend work proceeds. Use shared existing custom CLI constructors for `ac`; managed constructor for `am`.

All writers work in separate prepared worktrees based on 190906e. Early completed contract files may be copied between worktrees by root after writer agreement. Do not commit. Root integrates finished patches sequentially. Do not run broad/full cluster suites concurrently. Use focused Make targets and `make fmt`; full seeded E2E comes last and full gates run before commits.

## Runtime pins and constraints

Verified pins are CNPG Helm 0.29.1 / operator 1.30.1, Barman plugin Helm 0.8.1 / plugin 0.15.1, Barman 3.20.1. Both charts use https://cloudnative-pg.github.io/charts. Exact image pin must be verified before use. Existing legacy target CNPG must not be silently adopted or run beside another watcher. Current Borq clusters have no CSI snapshots; preferred mode reports unsupported, required mode fails before provisioning. S3 full backup/WAL remains mandatory.

The runtime evidence and upstream URLs are in /tmp/borq-managed-runtime/report.md. Use explicit Borq kubeconfig paths, never the unrelated default context. Weekly CronJob invokes narrowly scoped restore-check execution, not an unrestricted management token. Restore/check destinations use fresh identities and archives. Retention must never delete a live PVC or source archive as a cleanup shortcut.

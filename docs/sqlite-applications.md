# SQLite applications

Select **Enable SQLite** when creating an application.
Borq mounts a persistent 1 GiB volume at `/var/lib/borq/sqlite` and exports:

```text
SQLITE_DATABASE_PATH=/var/lib/borq/sqlite/app.sqlite
```

This is Borq's convention. Configure the app's connection to read this environment
variable; SQLite does not read it automatically. The app image must already contain
its SQLite library or driver. Apps that already support SQLite need no Dockerfile
changes. Borq supplies storage and Litestream in separate containers.

```yaml
apps:
  - name: web
    image: example/sqlite-app:v1
    sqlite:
      enabled: true
```

Enabling storage defaults `sqlite.backups` to `true`. Apps use zero or one replica
and Recreate deployments. Kubernetes [native sidecars](https://kubernetes.io/docs/concepts/workloads/pods/sidecar-containers/)
require Kubernetes 1.29 or newer with `SidecarContainers` enabled; this feature is
stable in 1.33. Use a StorageClass that supports SQLite filesystem locking and WAL
operations. Pod rescheduling with a surviving volume depends on its storage driver.

Litestream shares the database directory with the app. It restores a missing
database before the app starts and continuously uploads snapshots and WAL segments
to management RustFS, with seven days of retention and a snapshot every 24 hours.
This is asynchronous backup, not active database replication between nodes.
Borq generates a stable encryption key for each workload in OpenBao before
replication starts. Litestream encrypts snapshots and WAL before uploading them.
Restore uses the same retained key; a missing or wrong key fails rather than
initializing an empty replacement database. The key is projected through a
private credential Secret, and configuration files contain only environment
references. This encrypts backup objects; the live SQLite file on its volume
is not encrypted by Litestream.
An existing database file takes precedence over its remote backup. A missing database
is restored to a staging file and must pass SQLite's `PRAGMA integrity_check` before
it replaces the live file. A new app may initialize an empty database once. That
authorization is recorded outside the volume in an application-specific ConfigMap
and durably consumed in management state. Later volume loss without a usable backup
blocks startup with a recovery-required message. Reapplying a deployment or losing
that ConfigMap does not authorize an empty replacement database.

Each backup-enabled app also gets a weekly Sunday 03:00 UTC restore-verification
CronJob. It restores into temporary storage, runs `PRAGMA integrity_check`, and reports
success or failure through Kubernetes Job status and logs. It never mounts the live
volume. Disabling backups suspends this job. Notification routing remains separate.

The volume uses filesystem group 2000 and group-writable database files, allowing
apps to run under a non-root UID. Keep the whole directory mounted so adjacent WAL
and shared memory files share its filesystem. Predeploy commands receive the same
path and volume. Jobs use the existing volume consumer's node when present. Clear
the predeploy command before scaling a backup-enabled SQLite app to zero; a
migration without a subsequent app process would have no continuous backup worker.

RustFS remains on management. Creating a space queues a dedicated, randomly named
bucket and a service account restricted to that whole bucket. The space API's
`backupStorage` reports `pending`, `ready`, or `failed`; failed provisioning retries
in the background and survives manager restarts. Each SQLite app additionally gets
credentials restricted to `sqlite/SPACE_ID/APP_NAME` under that space's storage
prefix. Management administrator credentials stay in the management vault. Automatic provisioning
requires bucket creation and the RustFS admin API; an external S3 provider without
that API is not supported by this feature.

For another cluster, configure a management S3 endpoint reachable from both Borq
and application pods. An internal `.svc` hostname works within its cluster;
cross-cluster access needs a routable endpoint. Use HTTPS across untrusted networks.
No separate storage-encryption keys are generated per space. Local encryption remains
part of the management RustFS policy.

When [offsite backups](offsite-backups.md) are enabled, management also exports
retained SQLite buckets into independently encrypted Restic snapshots, with
retention scoped to each source bucket. Only generations with captured snapshots
are included. Changed object identities or missing captured bytes fail the export.
Offline rehydration preserves Litestream's file layout. Save its completion manifest
with `management backups restore --manifest space-manifest.json`, then use
`management backups local rebind-space SPACE_ID --manifest space-manifest.json`
while management is paused for recovery. This selects one generation per app using
the captured original object times, copies and verifies it into a fresh live prefix,
and reconnects management's saved space and archive settings. Equal or missing
chronology requires an explicit `--generations web=GENERATION_ID` selection. Recovered
historical generations remain intact in the original rehydrated prefix and Restic
snapshot. See the [space recovery procedure](offsite-backups.md#recover-space-buckets).

Offsite export is currently limited to 100,000 captured objects and a 64 MiB
completion manifest per source bucket. SQLite apps in the same space share this
limit. Frequent writes produce many WAL segments, so sustained workloads can hit
the cap within the seven-day retention window. Such an export fails and retains
the previous offsite checkpoint; local Litestream replication continues. Monitor
offsite status before relying on this setup for sustained production writes.

Set `sqlite.backups: false` to stop uploads while retaining the volume, database
and previous backups. Existing storage cannot be removed through an app update.
Primary storage removal, relocation and parent credential replacement are blocked
while SQLite destinations are retained. Automatic storage migration and parent
credential rotation are not implemented for these destinations yet.

Borq pins the official Litestream 0.3.14 image by digest, using the older backend
because of the unresolved [0.5 crash/restore report #1580](https://github.com/benbjohnson/litestream/issues/1580)
and [proposed fix #1581](https://github.com/benbjohnson/litestream/pull/1581), checked
on 27 October 2026. That image also has an old runtime and dependencies. This is a
compatibility choice, not a guarantee against security or recovery defects. Review
the release before relying on it for critical production data. See upstream's
[Kubernetes guide](https://litestream.io/guides/kubernetes/) and
[S3-compatible storage guide](https://litestream.io/guides/s3-compatible/).

`make test-sqlite-e2e` uses isolated namespaces on local `borq-dev` to check non-root
writes, cross-space bucket isolation, a predeploy command, lost-volume recovery,
offsite rehydration and rebinding, actual integrity-check jobs, backup suspension,
persistent manager settings and refusal to initialize an empty replacement database.

# Back up PostgreSQL archives to external S3

Borq can stream managed PostgreSQL S3 archives into encrypted, incremental Restic snapshots on an external S3 provider. Continuous base backups and WAL remain in local shared storage. Restoring a snapshot first rehydrates its objects into ordinary S3 for CloudNativePG recovery.

This protects S3 archive objects only. SQLite state and OpenBao's PebbleDB data need separate native backups. A Restic snapshot is not a complete management-cluster backup.

## Retain private recovery material before setup

Create a private `recovery.json` outside the management cluster. Supply your own repository password and external provider credentials. Keep this file in independent secure storage. Borq stores a copy in OpenBao but never generates a password whose only copy would be lost with the cluster.

Use a different external S3 bucket from the shared source bucket. The repository bucket must already exist. Borq initializes a missing Restic repository, but does not force-unlock an existing repository or initialize one after an unrelated connection failure.

```json
{
  "offsite": {
    "repository": "s3:https://s3.example.com/external-backups/borq",
    "region": "us-east-1",
    "password": "REPLACE_WITH_INDEPENDENTLY_RETAINED_PASSWORD",
    "accessKey": "REPLACE_WITH_PROVIDER_ACCESS_KEY",
    "secretKey": "REPLACE_WITH_PROVIDER_SECRET_KEY"
  },
  "source": "",
  "destination": {}
}
```

Restrict access to the file.

```sh
chmod 600 recovery.json
```

## Enable periodic export

Add an `offsite` block to the existing `management.yaml`.

```yaml
offsite:
  enabled: true
  interval: 6h
  keepLast: 14
```

Apply the policy using the private file.

```sh
borq management apply -f management.yaml --recovery-config recovery.json
borq management export > management-safe.yaml
```

The accepted operation contains an immutable `credentialsRef` instead of key material. The safe export includes that reference and `offsite.status`. Omission of `offsite` leaves the current policy unchanged. To stop future attempts, apply `offsite: {enabled: false}`. An already accepted attempt can finish.

The manager owns one worker and resumes due or interrupted work after restart. A failed attempt retains the previous successful snapshot, source, repository, and credential reference. Retention runs after a successful capture and applies only to that source's host and tag. Changing a policy starts a new due attempt without relabeling the last successful snapshot.

Inspect `offsite.status` in the safe export. Copy the `source` identity from the first successful checkpoint into the independently retained `recovery.json`. Keep the private file for every repository and password still needed by retained snapshots. A new source endpoint or root has its own snapshot history and retention scope.

An empty source or one with no complete PostgreSQL backup fails the attempt. A newly enabled policy therefore does not claim protection before the first recoverable base backup exists.

## Recover without the original management cluster

Use the same versioned Borq server image, which bundles Restic 0.18.0, or install that Restic version alongside the Borq CLI on Linux or macOS. The image pins the upstream multi-platform Restic digest. Recovery needs the private configuration and external S3 access. It does not need the original SQLite database, OpenBao service, or source S3 service.

Supply fresh destination credentials and a new, empty destination prefix in the private file.

```json
{
  "offsite": {
    "repository": "s3:https://s3.example.com/external-backups/borq",
    "region": "us-east-1",
    "password": "YOUR_RETAINED_PASSWORD",
    "accessKey": "YOUR_EXTERNAL_PROVIDER_ACCESS_KEY",
    "secretKey": "YOUR_EXTERNAL_PROVIDER_SECRET_KEY"
  },
  "source": "SOURCE_ID_FROM_THE_LAST_SUCCESSFUL_CHECKPOINT",
  "destination": {
    "endpoint": "https://recovery-s3.example.com",
    "region": "us-east-1",
    "bucket": "recovery",
    "prefix": "unpublished-2026-10-26",
    "pathStyle": true,
    "accessKey": "NEW_DESTINATION_ACCESS_KEY",
    "secretKey": "NEW_DESTINATION_SECRET_KEY"
  }
}
```

List source-scoped snapshots, then restore a full snapshot ID or `latest`.

```sh
borq management backups list --config recovery.json
borq management backups restore SNAPSHOT_ID --config recovery.json
```

Publish the destination to CloudNativePG only after the restore command succeeds. A failed command can leave isolated partial objects and a reservation marker. Use another fresh prefix for a retry. Borq never overwrites existing destination objects. Successful recovery requires a valid completion manifest, matching object sizes and SHA-256 digests, and a successful Restic dump process exit.

The restored relative layout is unchanged.

```text
organizations/ORGANIZATION_ID/postgres/ARCHIVE_ID/archive-ARCHIVE_ID/base/BACKUP_ID/...
organizations/ORGANIZATION_ID/postgres/ARCHIVE_ID/archive-ARCHIVE_ID/wals/...
```

For Barman recovery, use the following mapping.

| Setting | Value |
| --- | --- |
| `destinationPath` | `s3://recovery/unpublished-2026-10-26/organizations/ORGANIZATION_ID/postgres/ARCHIVE_ID` |
| `serverName` | `archive-ARCHIVE_ID` |
| Recovery backup ID | The selected `BACKUP_ID` from the captured catalogue |

A restored database needs a new live backup destination and its own database credentials. The captured catalogue proves the base backup and required consistency WAL. Its recovery boundary reaches later complete backups only when the shared catalogue evaluator establishes continuous WAL between them. It does not promise arbitrary recovery to the newest uploaded WAL time.

## Size and consistency limits

Export captures only known managed PostgreSQL archive namespaces. It excludes mutable Loki objects, unrelated organization objects, and incomplete concurrent base backups. Every captured body uses its listed ETag in a conditional GET. Missing or changed required objects fail the producer, so Restic publishes no snapshot for that attempt.

The deterministic GNU tar stream supports large physical archives and ends with a versioned completion manifest. Export and recovery both reject objects larger than 625 GiB. Uploads use size-aware parts between 8 MiB and 64 MiB. The captured inventory is limited to 100000 objects, backup metadata to 32 MiB in total and 1 MiB per `backup.info`, and the terminal manifest to 64 MiB. Individual S3 transfers have a two-hour deadline. Manager attempts and offline recovery have a 24-hour deadline. No second full archive volume is required. The manager reuses a private Restic metadata cache under its data directory, scoped to the source. Standalone list and recovery use private temporary metadata caches. These caches contain no source-object mirror.

Source pruning during capture can fail an attempt. Borq retries on the next due interval and keeps the preceding successful checkpoint. External repository storage remains the operator's responsibility, including provider access, independent credential custody, and repository recovery.

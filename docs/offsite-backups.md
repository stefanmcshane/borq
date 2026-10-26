# Back up and recover management state

Borq takes independent native SQLite and OpenBao Raft snapshots into shared S3, then streams completed management artifacts and PostgreSQL archives into encrypted Restic snapshots on external S3. Local capture and offsite export have separate schedules. SQLite and OpenBao start from the same local tick, with their own capture times, results, retention and previous successful checkpoints. Recovery accepts their possible time skew; the snapshots are not an atomic group.

Each successful native capture prunes that component's older completed snapshots. Failed publication removes its completion marker before removing the payload. Retention also removes abandoned payloads without markers after 24 hours, allowing active captures and offsite rehydration to finish.

Both profiles use durable, single-node OpenBao Raft. This is a local-disk baseline without HA. There is no migration from development PebbleDB or ephemeral storage.

## Retain recovery material independently

Generate a private age identity outside the management cluster. The command writes a new mode-0600 file and prints only its public recipient.

```sh
borq management backups local keygen --identity-file management.agekey
```

Keep the identity in independent secure storage. Put only the printed `age1...` public recipient in management configuration. Capture needs no decryption identity.

Also retain the original OpenBao administrative token and unseal keys outside the cluster. Bootstrap keeps them in the Kubernetes Secret `borq/borq-openbao-admin`; that Secret is not included with the S3 snapshots. Save a private recovery copy before losing the cluster, for example:

```sh
umask 077
kubectl --kubeconfig management.yaml -n borq get secret borq-openbao-admin -o json > openbao-admin.private.json
```

Protect this file like a root credential. After recovery, the original token and unseal keys must replace the fresh cluster's temporary bootstrap material. Do not upload these keys beside the native snapshots.

Native SQLite payloads contain privileged cluster connections. Borq encrypts both native payload types with age before publishing them. This protects confidentiality and detects altered ciphertext through authenticated EOF. It does not authenticate the publisher or isolate S3 permissions: PostgreSQL clients currently share the built-in RustFS root credentials and can delete or overwrite shared objects. Separately credentialed external storage protects against loss of that shared store. Native encryption is randomized, so repeated native snapshots have limited cross-snapshot deduplication; PostgreSQL archive deduplication remains incremental.

Create a private `recovery.json` outside Borq with your own Restic password and external provider credentials. Keep every password/configuration still needed by retained snapshots. Borq stores a copy in OpenBao but does not generate a password whose only copy is in the lost cluster.

```json
{
  "offsite": {
    "repository": "s3:https://s3.example.com/external-backups/borq",
    "region": "us-east-1",
    "password": "INDEPENDENTLY_RETAINED_PASSWORD",
    "accessKey": "EXTERNAL_PROVIDER_ACCESS_KEY",
    "secretKey": "EXTERNAL_PROVIDER_SECRET_KEY"
  },
  "source": ""
}
```

Use a different bucket from the shared source bucket. The repository bucket must already exist. Borq initializes a missing repository, without force-unlocking a repository or treating an unrelated connection failure as an empty repository. Restrict the private file to its owner:

```sh
chmod 600 recovery.json
```

## Enable local capture and offsite export

Use the existing management apply/export flow:

```yaml
backups:
  enabled: true
  interval: 1h
  keepLast: 24
  recipient: age1REPLACE_WITH_PRINTED_PUBLIC_RECIPIENT
offsite:
  enabled: true
  interval: 6h
  keepLast: 14
```

```sh
borq management apply -f management.yaml --recovery-config recovery.json
borq management export > management-safe.yaml
```

`backups.status.components.sqlite` and `.openbao` report independent results. A failed component retains its own previous artifact and original safe storage location/reference; success of the other component remains visible. Retention removes older completed artifacts per component only after a new success. An interrupted attempt resumes after restart, including a hard crash with a future next-attempt time. The local worker runs independently of long offsite exports; manager shutdown cancels and joins both workers.

Omitting either block leaves that policy unchanged. Apply `backups: {enabled: false}` or `offsite: {enabled: false}` to stop future starts. An already accepted attempt can finish. Credentials never appear in safe configuration, SQLite policy, queued operations or status. Only the public age recipient and immutable credential references are durable there.

Completed native objects have this layout relative to the shared S3 root:

```text
management/sqlite/IMMUTABLE_ID/payload
management/sqlite/IMMUTABLE_ID/complete.json
management/openbao/IMMUTABLE_ID/payload
management/openbao/IMMUTABLE_ID/complete.json
```

The encrypted payload is published before its completion marker. Partial payloads are excluded from listing and offsite capture. Missing or changed payloads with a completion marker fail validation. An empty source fails offsite capture; a source with valid management artifacts and no PostgreSQL backup is supported.

Offsite status retains its last successful source, repository and credential reference. Copy its `source` identity into your private recovery file after the first success. Retention is scoped to that source's Restic host/tag; a new source endpoint/root has a separate history.

## Rehydrate external backups into fresh S3

Use the matching Borq release image, which bundles pinned Restic 0.18.0, or install that version alongside the CLI on Linux/macOS. Recovery needs the independent private configuration and external S3 access. It does not require the original SQLite database, OpenBao or source S3 service.

Bootstrap a fresh RustFS/S3 service with independently supplied credentials and create its bucket. Do not make bootstrap depend on the missing vault. Add fresh destination access and a new unpublished prefix to `recovery.json`:

```json
{
  "offsite": {
    "repository": "s3:https://s3.example.com/external-backups/borq",
    "region": "us-east-1",
    "password": "RETAINED_PASSWORD",
    "accessKey": "EXTERNAL_PROVIDER_ACCESS_KEY",
    "secretKey": "EXTERNAL_PROVIDER_SECRET_KEY"
  },
  "source": "SOURCE_ID_FROM_SUCCESSFUL_CHECKPOINT",
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

```sh
borq management backups list --config recovery.json
borq management backups restore SNAPSHOT_ID --config recovery.json
```

Use a full snapshot ID or `latest`. Publish/use the destination only after success. Failure can leave partial objects and a reservation marker; use another fresh prefix for retry. Existing objects are never overwritten. Success requires the terminal manifest, sizes, hashes, captured catalogue and successful Restic producer exit.

## Recover native components while workers stay paused

For local recovery, supply the existing local S3 connection. After offsite rehydration, set `local` to the fresh `destination` connection. Include the retained private age identity and original OpenBao material in the private file:

```json
{
  "local": {
    "endpoint": "https://recovery-s3.example.com",
    "region": "us-east-1",
    "bucket": "recovery",
    "prefix": "unpublished-2026-10-26",
    "pathStyle": true,
    "accessKey": "NEW_DESTINATION_ACCESS_KEY",
    "secretKey": "NEW_DESTINATION_SECRET_KEY"
  },
  "identity": "AGE-SECRET-KEY-REPLACE_WITH_RETAINED_IDENTITY",
  "openbao": {
    "address": "http://127.0.0.1:8200",
    "originalToken": "RETAINED_ORIGINAL_ADMIN_TOKEN",
    "unsealKeys": ["RETAINED_ORIGINAL_UNSEAL_KEY"]
  }
}
```

Keep Borq stopped. Run a fresh single-node Raft OpenBao using the release's management chart values; a port-forward can supply the explicit recovery address. The CLI initializes and temporarily unseals an uninitialized instance, force-restores the native snapshot, then unseals and authenticates with the original material. For an already initialized fresh instance, unseal it with its temporary keys and supply `openbao.bootstrapToken`. Recovery intentionally replaces the selected instance's state.

```sh
borq management backups local list --config recovery.json
borq management backups local restore sqlite SQLITE_ID --data-dir /var/lib/borq-recovered --config recovery.json
borq management backups local restore openbao OPENBAO_ID --config recovery.json
borq management backups local inspect --data-dir /var/lib/borq-recovered
borq management backups local rebind --data-dir /var/lib/borq-recovered --config recovery.json
```

Choose each component's artifact independently by its actual capture time. Every restore authenticates the entire ciphertext before installing SQLite files or invoking Raft restore. SQLite restoration requires a fresh directory and includes flattened kubeconfigs for registered clusters and unfinished management tasks. Unsupported external credential plugins fail capture clearly; replace those connections with independently recoverable credentials. SQLite is checked for integrity before publication.

The restored directory contains `RECOVERY_REQUIRED`. Normal Borq startup refuses to open it, so old queued operations cannot automatically affect clusters. `inspect` shows operation identities/states without exposing their payloads. `rebind` works while the barrier remains: it validates the new S3 connection, replaces the recovered shared connection and matching preserved PostgreSQL archive locations, and updates `secret/borq/platform/rustfs` in the restored vault with the new credentials. Historical archives from different stores need separate review. Rebinding is retryable while operations remain paused.

Before resume:

1. Restore `borq-openbao-admin` in the fresh management namespace from your independently retained original token/unseal keys. Remove old Secret metadata (`uid`, `resourceVersion`, `creationTimestamp`, `managedFields`) before applying it; keep the Secret name and use the fresh namespace.
2. Review saved target kubeconfigs in `clusters/`, and replace `clusters/management.yaml` with the fresh management connection. Start Borq with `--management-kubeconfig /var/lib/borq-recovered/clusters/management.yaml` when running outside the cluster, or use its fresh service account inside the cluster.
3. Review queued operations, advertised component endpoints and retained archive locations. Rebinding storage does not make old deployment endpoints valid. Keep the server stopped until replay is acceptable.
4. Explicitly permit startup:

```sh
borq management backups local resume --data-dir /var/lib/borq-recovered
borq server --data-dir /var/lib/borq-recovered --management-kubeconfig /var/lib/borq-recovered/clusters/management.yaml
```

Native restore proves database/connection and Raft-state recovery. Normal cluster operation still requires the operator's connection review and explicit resume; Borq does not claim coordinated point-in-time consistency between the components.

## Recover PostgreSQL

The relative archive layout remains:

```text
organizations/ORGANIZATION_ID/postgres/ARCHIVE_ID/archive-ARCHIVE_ID/base/BACKUP_ID/...
organizations/ORGANIZATION_ID/postgres/ARCHIVE_ID/archive-ARCHIVE_ID/wals/...
```

| Barman setting | Rehydrated value |
| --- | --- |
| `destinationPath` | `s3://recovery/unpublished-2026-10-26/organizations/ORGANIZATION_ID/postgres/ARCHIVE_ID` |
| `serverName` | `archive-ARCHIVE_ID` |
| Recovery backup ID | Selected `BACKUP_ID` from the captured catalogue |

A recovered database needs a new live backup destination and its own database credentials. The shared catalogue evaluator establishes the base backup, required consistency WAL and continuous coverage between later completed backups. It does not promise arbitrary recovery to the newest uploaded WAL time.

## Bounds and capture consistency

Exports include completed native artifacts and known managed PostgreSQL namespaces, excluding mutable Loki objects, unrelated data and incomplete concurrent base backups. Every captured body uses its listed ETag in a conditional GET and is hashed while streaming. Missing/mutating required objects fail the producer; Restic creates no snapshot for that attempt.

The deterministic GNU tar stream supports large PostgreSQL archives, with a versioned terminal manifest. PostgreSQL objects are limited to 625 GiB; multipart uploads use 8–64 MiB parts. Native ciphertexts are limited to 8 GiB (plaintext reserves 8 MiB for encryption framing); native restore uses private temporary files and authenticates EOF before native consumption. SQLite connections are limited to 10000, 1 MiB per kubeconfig and 16 MiB encoded in total. Native completion markers are limited to 64 KiB each and 32 MiB total; PostgreSQL `backup.info` has separate 1 MiB/32 MiB bounds. Inventory is capped at 100000 objects and the terminal manifest at 64 MiB.

Transfers have bounded deadlines. Local component attempts have 30 minutes; offline native recovery has two hours. Offsite attempts/recovery have 24 hours. No second full PostgreSQL archive volume is staged. The manager keeps a private, source-scoped Restic metadata cache under its data directory; standalone recovery uses private temporary caches. External repository storage, access control, retained keys and recovery testing remain the operator's responsibility.

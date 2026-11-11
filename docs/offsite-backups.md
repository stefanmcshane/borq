# Back up and recover management state

Borq takes independent native SQLite and OpenBao Raft snapshots into shared S3, then streams completed management artifacts and PostgreSQL archives into encrypted Restic snapshots on external S3. Local capture and offsite export have separate schedules. SQLite and OpenBao start from the same local tick, with their own capture times, results, retention and previous successful checkpoints. Recovery accepts their possible time skew; the snapshots are not an atomic group.

Use **Offsite copy of S3 data** in **Settings** to configure the separate external
S3 destination, schedule, retention, repository password, and provider credentials.
Keep the password and provider credentials outside Borq for recovery. Management
configuration also supports these settings, as shown below.

In **Offsite S3 destination**, enter the bucket's HTTPS address and optional folder,
for example `https://s3.us-east-1.amazonaws.com/my-backups/borq`.
Borq adds the backup tool's `s3:` prefix internally. Management and recovery
configuration files still use the full `s3:https://...` repository format.

Enter the encryption password and S3 provider credentials in the backup form.
Borq stores them in OpenBao and manages credential references internally.
Later saves reuse the stored credentials when those fields are blank.

Choose **Test and save** to test S3 list, write, read, and delete access in the
background. Borq saves the connection settings only after the test succeeds.
A failed test leaves the saved settings active. The API and MCP use the same
background operation and report its progress through `GetManagementOperation`.
Disabling offsite backups does not require a working connection.

Primary S3 storage defaults to local persistent RustFS for live management
services and backup archives. Its
bucket is named after the management cluster: `borq-<management-cluster-name>-data`.
For the default `management` cluster, this is `borq-management-data`.
Configure the primary connection through the `s3` block in management configuration
or the management API.
Saving primary storage settings alone does not enable offsite backups.

The offsite worker also captures every provisioned space bucket, including
PostgreSQL-only spaces and [application SQLite](sqlite-applications.md), plus each
organization-managed Pocket ID bucket after its SQLite backups are configured.
Pocket ID buckets remain included if a later provider reconciliation fails.
Each source bucket has its own tagged Restic history and retention.
Before exporting, Borq freezes the exact object inventory for every source,
checks retained SQLite encryption keys, and captures a fresh OpenBao snapshot.
Each source's completed Restic archive includes that encrypted OpenBao checkpoint
with its workloads. Later object arrivals wait for the next export. Source
retention therefore keeps a matching vault checkpoint with each workload archive.
SQLite exports include Litestream generations with captured snapshots and preserve
their object layout for offline rehydration. These differ from the native Borq
management SQLite snapshots described below. Offsite copies include recognized
backup objects; they do not mirror arbitrary RustFS buckets or objects.

**Disable offsite backups** stops future copies and retains existing offsite
snapshots and stored credentials. Disabling leaves primary S3 data in place.

| Configuration | Purpose | Destination |
| --- | --- | --- |
| `s3` | Primary backup archive storage connection | Persistent RustFS by default, or an explicitly configured S3 service |
| `backups` | Scheduled encrypted native SQLite and OpenBao captures | The primary `s3` archive store |
| `offsite` | Scheduled encrypted incremental Restic exports of completed management backups and PostgreSQL archives | A separate external S3 repository |

Each successful native capture prunes that component's older completed snapshots. Failed publication removes its completion marker before removing the payload. Retention also removes abandoned payloads without markers after 24 hours, allowing active captures and offsite rehydration to finish.

Both profiles use durable, single-node OpenBao Raft. This is a local-disk baseline without HA. There is no migration from development PebbleDB or ephemeral storage.

## Check backup coverage

After logging in, run:

```sh
borq management backups status
```

Or open **Settings → Backup coverage → Check backup coverage**. The check reads
archive metadata without changing storage or creating encryption keys. Use
`--output json` for structured CLI output.

The report includes Borq SQLite, OpenBao, each configured SQLite application,
organization and Space Pocket ID, managed PostgreSQL, and a separate offsite row.
It includes disabled and incomplete backup setups. Removed managed databases
are excluded; their retained archives remain available through recovery commands.

| Status | Meaning |
| --- | --- |
| `available` | Current archive evidence was found, or the offsite worker recorded a successful export for its current destination. |
| `disabled` | Backup creation or offsite export is disabled. |
| `missing` | No complete backup evidence was found, or backup storage is not ready. |
| `stale` | Backup evidence is older than its freshness deadline. |
| `failed` | The latest capture, export, or retention attempt failed. A previous backup may still exist. |
| `unknown` | The archive or its credentials could not be inspected. Check storage and OpenBao access. |

Native management backups and offsite exports become stale after two configured
intervals without success. SQLite workload archives become stale after 48 hours
without a snapshot or WAL object in a generation containing a snapshot;
Litestream is configured for daily snapshots, so idle databases are not judged
against the one-second WAL sync interval. PostgreSQL requires a complete base
backup and its required WAL, and becomes stale one hour after the next scheduled
full backup. PostgreSQL dates come from database backup metadata, not S3 upload
times. JSON includes each observed backup date and freshness deadline.

An offsite success does not establish that every database has a backup. Check
the individual database rows. Archive inspection checks metadata and object
presence; it does not download every payload or prove that a restore succeeds.
Run the manual recovery procedure to verify that separately.

## Retain recovery material independently

Borq manages backup encryption automatically. Set `backups.enabled: true` to
schedule native Borq SQLite and OpenBao captures; no encryption key is supplied
in `management.yaml`. Borq derives a dedicated archive key from OpenBao's original
auto-unseal key, using a fixed versioned derivation, and stores that archive key
in OpenBao. The exported auto-unseal key can reconstruct it offline, including
when the original cluster and OpenBao are unavailable. `backups.encryptionKey`
reports only the managed public key.

After bootstrap, export the management credentials separately:

```sh
borq management keys
```

This creates `borq-management-recovery.json` with private permissions and prints
its absolute path. Keep a copy in secure storage outside the cluster and out of
Git before applying management configuration. It contains `borq.adminToken`,
`openbao.originalToken` and `openbao.sealKey`. The seal key unlocks OpenBao's
encrypted Raft data; it is separate from the administrator token used to access
OpenBao. Neither value can be recovered from an encrypted snapshot alone.
`--output-file PATH` selects another destination. Reruns reuse matching material
and refuse to overwrite an incomplete or different file. You can export the same
keys as often as needed without rotating them. Bootstrap and keys both use the
current Kubernetes context by default. Use `--kubeconfig PATH` to select another
file. If the namespace is missing, keys tells you to run bootstrap. If credentials
are still being created, it waits up to `--timeout` (five minutes by default) and
then explains how to retry and check the Borq pods. It can export as soon as both
credential Secrets are complete, before all supporting services are ready.

Use the printed private port-forward, then log in:

```sh
borq login --api-url http://127.0.0.1:7833 --token-file ./borq-management-recovery.json
borq management apply -f management.yaml
```

Login accepts a raw token file or this recovery file. It saves only the Borq
administrator token. Verify your secure backup, then remove the working recovery
file after login. Apply uses your saved login and does not need the recovery
file. Use organization sign-in and account API tokens after OIDC setup.

Borq stores these credentials in Kubernetes before starting OpenBao:
`borq-mgmt/borq-admin-auth` and `borq-mgmt/borq-openbao-admin`. OpenBao uses its
saved static seal key and administrator token to initialize itself; Borq does
not depend on receiving and saving a one-time initialization response. OpenBao
then auto-unseals on ordinary restarts. An interruption of OpenBao's own initial
self-initialization can require manual repair; Borq does not erase data or
replace recovery keys automatically. Protect access to these Kubernetes Secrets.
They are not included alongside native S3 snapshots.

`borq management bootstrap-from-restore` is a placeholder command. It returns
“not implemented yet” and makes no changes. The intended flow is:

1. Retrieve offsite backups using the independently retained S3 credentials and
   Restic password. The original RustFS or OpenBao installation is not needed
   to decrypt the offsite export; current tooling rehydrates it into temporary S3.
2. Restore OpenBao using its saved seal key and administrator token, so the other
   managed credentials become available.
3. Recover RustFS archives and Borq's SQLite database before resuming reconciliation.

Static-seal OpenBao restore automation is not implemented yet. Retain the
management recovery file and every user-supplied offsite password and provider
credential still needed by retained backups. No separate age private key is
required for new management snapshots.

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

Production installations enable native captures explicitly with `backups.enabled`.
Encryption is automatic. Enabling offsite export does not change the scheduled
local capture policy; it takes an additional fresh OpenBao checkpoint required
for each offsite archive's workload keys.
`make dev` enables hourly native captures with 24 retained artifacts per component
when no backup policy exists. Export the combined recovery file with
`borq management keys` using that development cluster's kubeconfig. An existing
policy, including an explicit disable, is preserved.

Use the existing management apply/export flow:

```yaml
backups:
  enabled: true
  interval: 1h
  keepLast: 24
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

Omitting either block leaves that policy unchanged. Apply `backups: {enabled: false}` or `offsite: {enabled: false}` to stop future starts. An already accepted attempt can finish. Credentials never appear in safe configuration, SQLite policy, queued operations or status. Only the public encryption key and immutable credential references are durable there.

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
borq management backups restore SNAPSHOT_ID --config recovery.json --manifest restored-manifest.json
```

Use a full snapshot ID or `latest`. Publish/use the destination only after success. Failure can leave partial objects and a reservation marker; use another fresh prefix for retry. Existing objects are never overwritten. Success requires the terminal manifest, sizes, hashes, captured catalogue and successful Restic producer exit.

## Recover native components while workers stay paused

New management installations use a static seal. You can decrypt their native
snapshots with the saved combined management recovery file, then restore Raft
with OpenBao's native CLI as described below. Automated installation recovery
through `management bootstrap-from-restore` remains unimplemented.

For local recovery, supply the existing local S3 connection. After offsite rehydration, set `local` to the fresh `destination` connection. For new management SQLite archives, include `openbao.sealKey` from the management recovery file. It reconstructs the archive decryption key offline; no separate `identity` is needed. For example:

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
  "openbao": {
    "address": "http://127.0.0.1:8200",
    "originalToken": "RETAINED_ORIGINAL_ADMIN_TOKEN",
    "sealKey": "RETAINED_ORIGINAL_AUTO_UNSEAL_KEY"
  }
}
```

Keep Borq stopped. The commands below restore SQLite into a paused directory.
For static-seal OpenBao, use `local decrypt` and the native restore procedure
below. The older `local restore openbao` helper supports only Shamir-initialized
instances with a historical `identity` and `unsealKeys`; it rejects static-seal
restore before contacting the target.

```sh
borq management backups local list --config recovery.json
borq management backups local restore sqlite SQLITE_ID --data-dir /var/lib/borq-recovered --config recovery.json
borq management backups local inspect --data-dir /var/lib/borq-recovered
borq management backups local rebind --data-dir /var/lib/borq-recovered --config recovery.json
```

Choose each component's artifact independently by its actual capture time. Every restore authenticates the entire ciphertext before installing SQLite files or invoking Raft restore. SQLite restoration requires a fresh directory and includes flattened kubeconfigs for registered clusters and unfinished management tasks. Unsupported external credential plugins fail capture clearly; replace those connections with independently recoverable credentials. SQLite is checked for integrity before publication.

The restored directory contains `RECOVERY_REQUIRED`. Normal Borq startup refuses to open it, so old queued operations cannot automatically affect clusters. `inspect` shows operation identities/states without exposing their payloads. `rebind` works while the barrier remains: it validates the new S3 connection, replaces the recovered shared connection and matching preserved PostgreSQL archive locations, and updates `secret/borq/platform/rustfs` in the restored vault with the new credentials. Historical archives from different stores need separate review. Rebinding is retryable while operations remain paused.

### Restore static-seal OpenBao manually

Use the original `borq-management-recovery.json` saved by `management keys`.
The `local` connection in `recovery.json` must reach the selected encrypted
payload and completion marker. If managed RustFS and OpenBao are both lost,
extract the checkpoint from Restic first using the procedure below. Upload its
verified `management/openbao/ID/payload` and `complete.json` into an independent
temporary S3 prefix, and point `local` at that prefix. That temporary S3 service
must start without the lost OpenBao KMS.

Create a private working directory. Write the original auto-unseal key and
administrator token into files, without printing their values:

```bash
umask 077
work=$(mktemp -d "${TMPDIR:-/tmp}/borq-static-recovery.XXXXXX")
mkdir "$work/bootstrap" "$work/data"
python3 -I - borq-management-recovery.json "$work/bootstrap" <<'PY'
import base64, json, os, re, sys
with open(sys.argv[1], encoding="utf-8") as source:
    material = json.load(source)["openbao"]
assert len(base64.b64decode(material["sealKey"], validate=True)) == 32
assert re.fullmatch(r"[0-9a-f]{64}", material["originalToken"])
for name, value in {"sealKey": material["sealKey"], "token": material["originalToken"]}.items():
    with open(os.path.join(sys.argv[2], name), "x", encoding="utf-8") as output:
        output.write(value)
PY
borq management backups local decrypt openbao OPENBAO_ID \
  --config recovery.json --keys-file borq-management-recovery.json \
  --output "$work/openbao.raft"
```

`decrypt` verifies the completion marker, ciphertext size, SHA-256, and age
authentication before creating the mode-0600 output. It refuses existing files
and symlinks. It does not print or save a separate backup identity.

Create a fresh OpenBao 2.7.1 instance with the original static seal and root
token. This example binds only to loopback and uses a new private Raft directory:

```bash
cat > "$work/server.hcl" <<EOF
disable_mlock = true
api_addr = "http://127.0.0.1:18200"
cluster_addr = "https://127.0.0.1:18201"
listener "tcp" {
  address = "127.0.0.1:18200"
  cluster_address = "127.0.0.1:18201"
  tls_disable = 1
}
storage "raft" {
  path = "$work/data"
  node_id = "openbao-0"
}
seal "static" {
  current_key_id = "borq-v1"
  current_key = "file://$work/bootstrap/sealKey"
}
initialize "borq" {
  request "administrator" {
    operation = "update"
    path = "auth/token/create-orphan"
    data = {
      id = {
        eval_source = "file"
        eval_type = "string"
        path = "$work/bootstrap/token"
      }
      policies = ["root"]
      no_default_policy = true
      ttl = "0"
    }
  }
}
EOF
bao server -config="$work/server.hcl"
```

In another terminal, set `work` to that same private directory. Wait for
`bao status` to report an initialized, unsealed instance, then restore with the
[official Raft snapshot command](https://openbao.org/docs/commands/operator/raft/):

```bash
export BAO_ADDR=http://127.0.0.1:18200
export BAO_TOKEN="$(cat "$work/bootstrap/token")"
bao status
bao operator raft snapshot restore "$work/openbao.raft"
bao status
bao token lookup -format=json >/dev/null
```

Use the original seal key before starting the fresh target. The normal restore
checks seal compatibility; this procedure does not use `-force`. Keep the
recovered vault private until you restore it into the final management instance
with the same static seal, administrator material, and fresh Raft storage.
Review the managed endpoints, TLS projections, storage access, and retained key
references before starting RustFS or Borq workers.

`make test-manual-recovery` creates and deletes its own source kind cluster.
It removes the source vault, S3 service, app writer, and plaintext files before
using the exported combined file with `local decrypt` and the official Bao CLI.
It then reopens Borq's recovered organization and restores an encrypted app
snapshot and WAL using only the identity read from the recovered vault.

### Resume reviewed management state

Before resume:

1. Restore `borq-openbao-admin` in the fresh management namespace from your independently retained original administrator token and auto-unseal key. Remove old Secret metadata (`uid`, `resourceVersion`, `creationTimestamp`, `managedFields`) before applying it; keep the Secret name and use the fresh namespace.
2. Review saved target kubeconfigs in `clusters/`, and replace `clusters/management.yaml` with the fresh management connection. Start Borq with `--management-kubeconfig /var/lib/borq-recovered/clusters/management.yaml` when running outside the cluster, or use its fresh service account inside the cluster.
3. Review queued operations, advertised component endpoints and retained archive locations. Rebinding storage does not make old deployment endpoints valid. Keep the server stopped until replay is acceptable.
4. Explicitly permit startup:

```sh
borq management backups local resume --data-dir /var/lib/borq-recovered
borq server --data-dir /var/lib/borq-recovered --management-kubeconfig /var/lib/borq-recovered/clusters/management.yaml
```

Native restore proves database/connection and Raft-state recovery. Normal cluster operation still requires the operator's connection review and explicit resume; Borq does not claim coordinated point-in-time consistency between the components.

## Recover space buckets

Each space has its own source identity and Restic history. List the repository's
snapshots and match their source tags against the saved space storage connection.
Restore the selected snapshot for each space into a dedicated fresh RustFS bucket
and unpublished prefix, saving its completion manifest with `--manifest`. Use a
separate recovery configuration per space: `local` must point to that rehydrated
bucket with administrator access, and `openbao` must identify the restored vault.
Run management `local rebind` first so its bucket is explicitly recorded. Keep
management stopped with its `RECOVERY_REQUIRED` barrier present.

Before rebinding, review and stop any surviving application predeploy Jobs in the
affected space (`borq.run/phase=predeploy`), and stop workloads still writing to old
storage. Offline rebinding cannot stop work running in a surviving target cluster.

```sh
borq management backups local rebind-space SPACE_ID \
  --data-dir /var/lib/borq-recovered --config space-recovery.json \
  --manifest space-manifest.json
```

The command verifies the manifest's source against the saved space connection,
selects the newest SQLite generation from original captured timestamps, and streams
all captured PostgreSQL objects plus one generation per app or Space Pocket ID provider into a fresh live
prefix. Every copy verifies object identity, size and SHA-256 before the saved state
is atomically rebound. For ambiguous chronology or an older intentional recovery,
pass `--generations web=GENERATION_ID,worker=GENERATION_ID`. Select a Space Pocket ID
generation with `--generations pocketid/PROVIDER_ID=GENERATION_ID`. Original rehydrated
objects remain intact. The rebound live prefix remains a complete source for future
offsite exports, including preserved PostgreSQL archives.

Rebinding creates bucket-limited space credentials and keeps replacement administrator
credentials only in the restored vault. It reconnects SQLite destinations and retained
PostgreSQL archive locations; it marks unfinished application deployments failed and
clears their saved queue so old storage settings cannot replay. After reviewing all
connections, explicitly resume management and create fresh application deployment
plans. Existing PostgreSQL databases lost with their target cluster must be restored
as new databases from the rebound catalogue. Management and space buckets must remain
distinct. This procedure introduces no automatic migration of existing installations.

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

A recovered database needs a new live backup destination and its own database credentials. The shared catalogue evaluator establishes the base backup, required consistency WAL and continuous coverage between later completed backups. A default restore replays all available archived WAL. A supplied target time must be reached by PostgreSQL itself; uploaded object timestamps do not prove a transaction recovery time.

## Bounds and capture consistency

Exports include completed native artifacts and known managed PostgreSQL namespaces, excluding mutable Loki objects, unrelated data and incomplete concurrent base backups. Every captured body uses its listed ETag in a conditional GET and is hashed while streaming. Missing/mutating required objects fail the producer; Restic creates no snapshot for that attempt.

The deterministic GNU tar stream supports large PostgreSQL archives, with a versioned terminal manifest. PostgreSQL objects are limited to 625 GiB; multipart uploads use 8–64 MiB parts. Native ciphertexts are limited to 8 GiB (plaintext reserves 8 MiB for encryption framing); native restore uses private temporary files and authenticates EOF before native consumption. SQLite connections are limited to 10000, 1 MiB per kubeconfig and 16 MiB encoded in total. Native completion markers are limited to 64 KiB each and 32 MiB total; PostgreSQL `backup.info` has separate 1 MiB/32 MiB bounds. Inventory is capped at 100000 objects and the terminal manifest at 64 MiB.

Transfers have bounded deadlines. Local component attempts have 30 minutes; offline native recovery has two hours. Offsite attempts/recovery have 24 hours. No second full PostgreSQL archive volume is staged. The manager keeps a private, source-scoped Restic metadata cache under its data directory; standalone recovery uses private temporary caches. External repository storage, access control, retained keys and recovery testing remain the operator's responsibility.

## Keys and recovery ownership

| Material | Used for | Stored by Borq | What the operator retains |
| --- | --- | --- | --- |
| Borq administrator token | Initial CLI login and management access | Kubernetes administrator Secret | Included in `borq-management-recovery.json`; use OIDC for daily access |
| OpenBao administrator token | Platform secret administration and recovery | Kubernetes `borq-openbao-admin` Secret | Included in the same recovery file |
| OpenBao auto-unseal key | Unlocks OpenBao's encrypted Raft data | Kubernetes `borq-openbao-admin` Secret | Included in the same recovery file; essential after cluster loss |
| Management archive identity | Encrypts native Borq SQLite and OpenBao snapshots before upload | OpenBao; derived with versioned HKDF from the auto-unseal key | No additional file: reconstructed offline from the recovery file |
| Application SQLite archive identity | Litestream encrypts snapshots and WAL before upload, including organization and Space Pocket ID | Separate stable key per workload in OpenBao, projected only into its backup credential Secret | Recovered through the accompanying OpenBao snapshot |
| PostgreSQL archive encryption | Barman requests AES256 for base backups and WAL; managed RustFS encrypts objects using OpenBao Transit | Transit root and wrapping metadata in OpenBao; RustFS uses a renewable scoped token | Recovered through the accompanying OpenBao snapshot |
| Optional CSI volume snapshot encryption | Storage-level PostgreSQL snapshots, separate from Barman S3 archives | Supplied by the CSI storage provider, not OpenBao | Retain provider recovery access and keys where applicable |
| Internal KMS TLS certificate and private key | Verifies and protects the RustFS-to-OpenBao connection | Owned Kubernetes TLS Secret; regenerable transport material | No additional recovery file |
| Pocket ID application encryption key | Pocket ID protects sensitive application fields | OpenBao, projected into Pocket ID's Secret | Recovered with OpenBao; distinct from its Litestream archive identity |
| Database passwords, RustFS credentials, workload and controller tokens | Service authentication and scoped storage access | OpenBao, with required Kubernetes Secret projections | Recovered with OpenBao |
| Offsite Restic repository password | Encrypts the complete offsite export before external S3 receives it | Supplied by the operator, stored in OpenBao for scheduled exports | Retain separately; needed to retrieve OpenBao's own snapshot |
| External S3, Cloudflare, Tailscale, registry and other integration credentials | Access to operator-owned external services | Supplied by the operator and stored in OpenBao where configured | Retain or be able to replace them through the external provider |

For an external primary S3 provider, AES256 encryption keys belong to that
provider's encryption service. The provider must acknowledge encryption; Borq
rejects a connection that does not. Those provider keys are not managed by
OpenBao. The OpenBao Transit integration applies to Borq's managed RustFS.

Encryption here protects backup objects. It does not apply SQLCipher encryption
to live SQLite files or encrypt PostgreSQL's live volume. Protect the underlying
nodes and persistent volumes separately. TLS certificates and renewable service
tokens are managed operational material, not additional files the operator must
retain for disaster recovery.

Keep the combined management recovery file and the user-supplied offsite password
and storage access outside the cluster. OpenBao's native snapshot can be decrypted
without a running OpenBao or RustFS KMS, avoiding a circular recovery dependency.
Automated disaster bootstrap remains unimplemented.

A cold RustFS process configured for Vault KMS requires OpenBao to start. For
local-only disaster recovery, keep Borq and backup writers stopped, start an
isolated RustFS recovery instance over the retained data with KMS disabled and
no workload-facing Service, and retrieve only the payload-encrypted OpenBao
snapshot through private access. After restoring OpenBao, restart normal RustFS
with KMS enabled before allowing PostgreSQL archive writes or reads. This is a
manual recovery step; bootstrap does not automate it. Offsite checkpoint
extraction below avoids needing a running RustFS at all.

For that local recovery procedure:

1. Stop Borq, backup workers, and the normal RustFS instance before attaching its
   retained data to another process. Keep workload access fenced throughout recovery.
2. Start one temporary RustFS instance over the same data with
   `RUSTFS_KMS_ENABLE=false`, the retained S3 access credentials, and no
   workload-facing Service. Use a private port forward for recovery access.
3. Download the completed `management/openbao/<artifact>/payload` and its completion
   marker. Verify the recorded size and checksum, then decrypt the age payload with
   the identity reconstructed from the exported auto-unseal key. Keep this instance
   restricted to reading native recovery artifacts.
4. Stop the temporary instance before restoring normal storage access. Restore the
   original OpenBao state, including the Transit root and its RustFS metadata.
5. Start normal RustFS with verified HTTPS KMS enabled. Verify an existing encrypted
   PostgreSQL object can be read before resuming backup writers and workload access.

The temporary reader does not change bucket encryption defaults or create a new
KMS root. `make test-rustfs-encryption` exercises this separation against the pinned
RustFS and OpenBao images. The full static-seal bootstrap command remains a
placeholder.

## Extract the vault checkpoint before rebuilding managed storage

Every scheduled offsite archive fixes its workload object inventory first, then
captures a fresh encrypted OpenBao snapshot. Its version 4 completion manifest
names that required `vaultCheckpoint`; the payload and completion marker are
embedded in the same Restic snapshot for each source, including dedicated Space
and Pocket ID buckets. Source-scoped retention therefore keeps the workloads and
their key checkpoint together. Missing retained workload keys stop a new capture.

A mixed archive containing PostgreSQL needs an available encryption service when
`borq management backups restore` writes those PostgreSQL objects to S3. That
command is not the first recovery step when the original managed RustFS and
OpenBao have both been lost. Extract the encrypted OpenBao checkpoint directly
to a private local directory first. Restic's own repository objects do not require
Borq's OpenBao KMS.

The following requires Bash, GNU tar, Python 3 and Restic. Supply the repository's
independently retained credentials through Restic's standard private environment
or password-file configuration. Do not put credentials in command arguments.
Choose the completed snapshot from `restic snapshots --host borq-management
--tag borq-source-SOURCE_ID`, and replace the two identifiers below.

```bash
set -euo pipefail
umask 077
snapshot="SNAPSHOT_ID"
source="SOURCE_ID"
working=$(mktemp -d "${TMPDIR:-/tmp}/borq-vault-checkpoint.XXXXXX")
mkdir "$working/extracted"

# pipefail also rejects a Restic producer that fails after emitting valid tar bytes.
(
  ulimit -f 65536 # 64 MiB, in Bash's 1024-byte units.
  restic dump "$snapshot" /objects.tar | tar -xOf - borq-manifest.json
) > "$working/manifest.json"

artifact_id=$(python3 -I - "$working/manifest.json" "$source" <<'PY'
import datetime, json, re, sys
with open(sys.argv[1], encoding="utf-8") as manifest_file:
    manifest = json.load(manifest_file)
artifact = manifest["vaultCheckpoint"]
assert manifest["version"] == 4 and manifest["source"] == sys.argv[2]
assert artifact["version"] == 1 and artifact["kind"] == "openbao"
assert re.fullmatch(r"[0-9a-f]{32}", artifact["id"])
assert re.fullmatch(r"[0-9a-f]{64}", artifact["sha256"])
assert type(artifact["size"]) is int and 0 < artifact["size"] <= 8 * 1024**3
instant = lambda value: datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
assert instant(artifact["capturedAt"]) >= instant(manifest["inventoryCapturedAt"])
assert artifact in manifest["native"]
payload = "management/openbao/" + artifact["id"] + "/payload"
assert any(item["key"] == payload and item["size"] == artifact["size"]
           and item["sha256"] == artifact["sha256"] for item in manifest["objects"])
print(artifact["id"])
PY
)

(
  ulimit -f 8388608 # 8 GiB per extracted file.
  restic dump "$snapshot" /objects.tar \
    | tar -xf - -C "$working/extracted" --no-recursion \
        --no-same-owner --no-same-permissions \
        "management/openbao/$artifact_id/payload" \
        "management/openbao/$artifact_id/complete.json"
)

python3 -I - "$working/manifest.json" "$working/extracted" <<'PY'
import hashlib, json, os, stat, sys
with open(sys.argv[1], encoding="utf-8") as manifest_file:
    artifact = json.load(manifest_file)["vaultCheckpoint"]
base = os.path.join(sys.argv[2], "management", "openbao", artifact["id"])
marker_path = os.path.join(base, "complete.json")
marker_info = os.lstat(marker_path)
assert stat.S_ISREG(marker_info.st_mode) and marker_info.st_size <= 64 * 1024
with open(marker_path, encoding="utf-8") as marker:
    assert json.load(marker) == artifact
path = os.path.join(base, "payload")
info = os.lstat(path)
assert stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600
assert info.st_size == artifact["size"]
digest = hashlib.sha256()
with open(path, "rb") as payload:
    for block in iter(lambda: payload.read(1024 * 1024), b""):
        digest.update(block)
assert digest.hexdigest() == artifact["sha256"]
PY
mv "$working/extracted" "$working/verified"
printf 'Verified encrypted checkpoint: %s\n' \
  "$working/verified/management/openbao/$artifact_id/payload"
```

Trust the extracted checkpoint only after all commands succeed. A failed command
can leave an incomplete private working directory; do not import it. The procedure
streams the archive twice and stores only its bounded manifest and selected native
checkpoint locally. It neither needs an S3 recovery destination nor decrypts the
checkpoint. Decryption uses the management identity derived from the retained
OpenBao auto-unseal key. Continue with the manual static-seal restore procedure
above. Automated `bootstrap-from-restore` remains unimplemented; extraction
alone does not restore a running installation.

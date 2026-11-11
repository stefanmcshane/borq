# Recover a Borq installation from offsite S3 backups

This is the manual recovery sequence for the management, PostgreSQL and Pocket ID
data supported by Borq, using the [backup example](../backup-with-cloudflare-tailscale/README.md).
Use a prepared replacement cluster, the matching Borq release, OpenBao 2.7.1,
Restic 0.18.0 and privately retained recovery material. Infrastructure and CLI
installation belong in the [main setup guide](../../README.md).

`borq management bootstrap-from-restore` is not implemented. Follow every stage
below. An S3 archive restore alone does not recover a running installation.

## 1. Fence the original installation

**Never let two installations write to the same live database, backup prefix or
Restic source history.** They can overwrite data, publish inconsistent archives
or prune each other's recovery history. Restic locking does not fence Borq,
PostgreSQL, Litestream or storage controllers.

Before allowing replacement writes:

1. Power off or isolate every original node, or revoke its write credentials and
   cluster access. An unreachable node is not proof that its writers have stopped.
2. Stop Borq, PostgreSQL, application writers, Pocket ID, Litestream sidecars,
   predeploy Jobs, CronJobs and offsite export workers on surviving nodes.
   Scaling only Borq to zero is insufficient.
3. Fence old Cloudflare and Tailscale controllers so they cannot republish the old
   installation. Keep public traffic away from the replacement until validation.
4. Deny changes to retained repository data during recovery. The recovery
   identity needs read/list access plus create/delete access only within the
   repository's `locks/` prefix. Borq's Restic commands acquire shared locks.
   Deny writes/deletes to `data/`, `index/`, `keys/`, `snapshots/` and `config`.
   Keep the replacement's export access denied until its destination is reviewed.

Use fresh management and space buckets and a unique unpublished prefix for each
restore attempt. Use a separate destination for replacement offsite exports.
Leave the old repository intact. Do not force-unlock an active Restic operation.

## 2. Choose the recovery checkpoints

Copy this directory outside the checkout and restrict credential files:

```sh
umask 077
chmod 600 *-recovery.json
borq management backups list --config management-recovery.json
borq management backups list --config space-recovery.json
```

Fill in the retained repository password, restricted recovery credentials and source IDs
first. Each management, space and organization Pocket ID bucket has its own source
identity and snapshot history. Record a full snapshot ID for each, rather than
allowing `latest` to change during the incident. Pocket ID recovery is described
in [its separate instructions](pocketid.md).

Retain `borq-management-recovery.json`, the original provider configuration, and
any separately backed-up application files. Native SQLite and OpenBao captures
have independent timestamps. Review their relationship to the selected workload
archives; they are not an atomic cluster snapshot. Choose vault checkpoints that
contain every selected workload's retained encryption keys.

## 3. Restore OpenBao before managed storage

When both OpenBao and RustFS are lost, do not start by rehydrating PostgreSQL
objects into managed RustFS. Those writes need the original vault's KMS state.

1. Follow [verified vault checkpoint extraction](../../docs/offsite-backups.md#extract-the-vault-checkpoint-before-rebuilding-managed-storage)
   for the selected Restic snapshot. It validates the terminal manifest, named
   `vaultCheckpoint`, object size and hash, and the completed Restic stream.
2. Make the verified encrypted payload and completion marker available through
   an independent temporary S3 prefix. This temporary storage must work without
   the lost OpenBao. Point the native recovery configuration's `local` block at
   that prefix.
3. Follow [static-seal OpenBao recovery](../../docs/offsite-backups.md#restore-static-seal-openbao-manually)
   to decrypt the checkpoint using `borq-management-recovery.json` and restore
   it through the native `bao operator raft snapshot restore` command. Start the
   fresh target with the **original** static seal and administrator token. Do not
   use `-force` or initialize a replacement identity with new keys.
4. Recover the final management OpenBao instance with that same Raft state and
   original seal. Restore its original administrator Secret and `bootstrap.hcl`
   using the private `management-secrets.yaml` template below. Verify the original token and key
   paths before starting RustFS. A temporary recovered vault alone is not the
   final management vault.
5. Prepare replacement managed RustFS against the restored OpenBao Transit KMS,
   including its TLS trust and renewable token. Verify encrypted object writes
   and reads. See [KMS recovery ordering](../../docs/offsite-backups.md#keys-and-recovery-ownership).

Fill `management-secrets.yaml` from the original exported recovery file:
`borq.adminToken`, `openbao.originalToken` and `openbao.sealKey`. Preserve the
provided `bootstrap.hcl` exactly, including its trailing newline. Apply these
Secrets before starting the final vault or Borq:

```sh
chmod 600 management-secrets.yaml
kubectl --context REPLACEMENT_CONTEXT apply -f management-secrets.yaml
```

The replacement Borq deployment must remain stopped while its restored database
is being prepared. If infrastructure preparation uses a temporary empty Borq
installation, it must have no workload registrations or backup/export policies;
stop it before copying the recovered management state. Do not reuse its freshly
generated vault keys in place of the retained originals.

## 4. Restore management SQLite with replay paused

Use a dedicated recovery workstation with no running local Borq server and an
`/var/lib/borq` path that does not yet exist. Its parent must be writable and trusted.
The restore command refuses existing directories, including empty ones. Restore at this
absolute path because recovered kubeconfig paths are retained in SQLite and the
replacement deployment also uses `/var/lib/borq`.

In `management-recovery.json`, set `destination` and `local` to the same fresh
management bucket and prefix. Set `openbao` to the recovered vault's private
address and original credentials. The destination must acknowledge encryption
for PostgreSQL archive objects.

```sh
borq management backups restore MANAGEMENT_SNAPSHOT_ID --config management-recovery.json --manifest management-manifest.json
borq management backups local list --config management-recovery.json
borq management backups local restore sqlite SQLITE_ARTIFACT_ID --config management-recovery.json --data-dir /var/lib/borq
borq management backups local inspect --data-dir /var/lib/borq
borq management backups local rebind --data-dir /var/lib/borq --config management-recovery.json
```

Keep `RECOVERY_REQUIRED` present. `local rebind` checks the new connection and
rebinds management archive locations. It does **not** rewrite target kubeconfigs,
disable recovered backup policies or cancel every queued operation.

Review the flattened kubeconfigs in `clusters/`, replacing connections to lost
clusters with the intended replacement connections. Keep original cluster and
space identities when rebinding their storage. Review every unfinished management,
PostgreSQL, addon and component operation from `inspect` before permitting replay.
If an operation cannot be reviewed safely, stop here. There is no generic offline
command that cancels every recovered queue.

The endpoint in `local` must be reachable from the eventual Borq server and
database Pods, not just the recovery workstation. Preserve any unrelated archive
locations separately; automatic management rebind only updates matching stores.

## 5. Restore every space bucket

For each space, fill in a separate `space-recovery.json`. Its `destination` and
`local` blocks must name the same fresh RustFS bucket and prefix, distinct from
the management bucket. The credentials need RustFS administration for rebinding.
An arbitrary S3 service without that API is insufficient for `rebind-space`.

```sh
borq management backups restore SPACE_SNAPSHOT_ID --config space-recovery.json --manifest space-manifest.json
borq management backups local rebind-space SPACE_ID --data-dir /var/lib/borq --config space-recovery.json --manifest space-manifest.json
```

Use the original space ID and its matching snapshot manifest. Rebinding copies
verified archives into a new live prefix and clears that space's application
deployment queue. It does not cancel all management or PostgreSQL operations.
Resolve ambiguous SQLite generations explicitly with `--generations`; do not
guess which generation contains the required data.

Repeat for every affected space. If used, restore organization-managed Pocket ID separately
using [pocketid.md](pocketid.md) before resuming management. It is not covered by
`rebind-space`. Restore arbitrary uploaded files from their independent backups.

## 6. Transfer management state and resume one writer

Prepare the replacement's `borq-data` PVC through the normal provisioning process
and keep `deployment/borq` at zero replicas. Restore `borq-admin-auth` with the
original Borq token too. Preserve the replacement's RBAC and correct mount paths.

The temporary Pod below mounts the Borq PVC without starting the server:

```sh
kubectl --context REPLACEMENT_CONTEXT -n borq-mgmt scale deployment/borq --replicas=0
kubectl --context REPLACEMENT_CONTEXT apply -f borq-data-recovery.yaml
kubectl --context REPLACEMENT_CONTEXT -n borq-mgmt wait --for=condition=Ready pod/borq-data-recovery --timeout=180s
```

The destination PVC must be empty. Retain any separate bootstrap state outside
it before this transfer; do not overwrite an existing database. With Bash's
`set -o pipefail`, copy the closed recovered directory including its barrier:

```bash
set -euo pipefail
test -f /var/lib/borq/RECOVERY_REQUIRED
kubectl --context REPLACEMENT_CONTEXT -n borq-mgmt exec borq-data-recovery -- sh -c 'test -z "$(ls -A /var/lib/borq)"'
tar -C /var/lib/borq -cf - . | kubectl --context REPLACEMENT_CONTEXT -n borq-mgmt exec -i borq-data-recovery -- tar -C /var/lib/borq -xf -
kubectl --context REPLACEMENT_CONTEXT -n borq-mgmt exec borq-data-recovery -- borq management backups local inspect --data-dir /var/lib/borq
```

Recheck source fencing, restored identity resources, target connections and queued
operations. The recovered offsite policy still contains its old credentials and
destination. Deny those writes at the storage or network boundary until the new
policy is applied. The barrier only pauses Borq; it does not fence external Pods.

After that review, permit exactly one replacement Borq writer:

```sh
kubectl --context REPLACEMENT_CONTEXT -n borq-mgmt exec borq-data-recovery -- borq management backups local resume --data-dir /var/lib/borq
kubectl --context REPLACEMENT_CONTEXT -n borq-mgmt delete pod borq-data-recovery --wait=true
kubectl --context REPLACEMENT_CONTEXT -n borq-mgmt scale deployment/borq --replicas=1
kubectl --context REPLACEMENT_CONTEXT -n borq-mgmt rollout status deployment/borq --timeout=300s
```

Use the existing login procedure with the recovered administrator token. Recover
private Cloudflare and Tailscale configuration and verify the original Pocket ID
issuer and accounts if used, or the original external OIDC client, before publishing public traffic. Keep application writers
stopped while database recovery finishes.

## 7. Restore PostgreSQL and switch the backend

```sh
borq am postgres backup list appdb --organization example --space production
```

Select an `eligible: true` entry and copy its opaque Borq `id` into
`restore-postgres.yaml`. This is not the raw Barman directory name. Adjust target
capacity as needed. Optionally add `recoveryTarget` with an RFC3339 timestamp.

```sh
borq am postgres backup restore appdb-restored -f restore-postgres.yaml --organization example --space production --timeout 30m
borq am postgres get appdb-restored --organization example --space production
```

Restore creates a new database, credentials and archive destination. Omission of
`recoveryTarget` replays all available archived WAL. Transactions newer than the
last completed offsite snapshot may be lost. Verify SQL access and application
data before replacing the backend's `env.name` with the restored database's new
`environmentGroup`; retain `version: latest`.

Create fresh application plans from the reviewed application YAMLs, apply them
once, and check health. Do not point the recovered backend at the old primary.
Do not create another empty `appdb` in place of the restored database.

## 8. Re-enable backups and cut over

Fill in `resume-backups.yaml` with a separate replacement Restic destination and
new independently retained password and credentials. Keep the original repository
protected against archive changes and intact. Permit access only to the new export destination before
applying this policy, so Borq can validate its credentials.

```sh
borq management apply -f resume-backups.yaml --timeout 30m
borq management backups status
borq management export > replacement-management-safe.yaml
```

Verify successful native and offsite
captures for every source and a PostgreSQL restore check, and retain the new source
IDs. Test frontend, backend, sign-in, private administration and public routing
before directing users to the replacement. Keep the original fenced permanently.

This recovers supported application and identity databases plus Borq's desired
state. Cloud resources, Kubernetes provisioning, uploaded files and registry
availability remain separate recovery responsibilities. Do not declare complete
recovery until each dependency and application has been verified.

# Managed PostgreSQL verification

The implemented contract is in `managed-postgres.contract.md`. The implementation uses one manager-owned SQLite lifetime and separate durable addon, archive, and operation records.

## Live recovery acceptance

`make test-managed-postgres-e2e` passed on 2026-10-24 against the explicit management kubeconfig. The initial run took 279.80 seconds and the final code rerun took 317.30 seconds, or 326.52 seconds including the Make build step. The test created a one-instance, 1Gi source through the public YAML/CLI, retrieved connection variables through the CLI, seeded four tables, requested a full production backup, selected the actual S3 catalogue entry, and restored a fresh cluster.

All canonical row hashes, duplicate multiplicities, table hashes and counts, column metadata, constraints, indexes, sequence definitions and sequence values matched. Post-restore write/read/rollback checks passed. The source remained unchanged. The real weekly CronJob template authenticated through its projected token, recovered an isolated copy, checked SQL, and removed its copy and volumes. CPU/memory samples, filesystem and SQL observations, and bounded logs were present. Both test addons and the test namespace were removed.

Barman Cloud 3.20.1 writes `size=None`; the live test exposed this and the catalogue now computes compressed size from actual base archive objects. The initial full backup and continuous WAL succeeded against the existing RustFS service. No second S3 service was added.

## Product and management surfaces

A separate reviewer rendered the live Settings and PostgreSQL details in Chromium. Both asynchronous metrics/log panels loaded, approved backups appeared, and credential fields were blank. Read-only CLI list/get/backup list/metrics/logs succeeded. Disk formatting now uses readable units and identifies filesystem capacity, which can be shared by local-path volumes.

Sparse management apply for RustFS succeeded without creating any new Helm revision or touching Loki. Management apply for CloudNativePG and Barman succeeded and waited for the target component work. Existing default controller releases are CNPG chart 0.29.1/operator 1.30.1 and plugin chart 0.8.1/plugin 0.15.1.

The management S3 API roundtrip passed after the final storage fixes. Inline credentials became a reference-only response, retained archives remained readable, and the original development credential reference was restored. Loki returned to one ready replica and the shared PodLogs CRD retained its `v1alpha2` stored version. This roundtrip reused working RustFS keys; the separate real ESO test proves replacement with different key material.

## Independent review

Independent lifecycle and surface reviews identified and closed sparse component selection, exact operation completion, staged kubeconfig cleanup, empty-list apply, session-token rotation, snapshot schedule field ownership and cleanup, controller serialization, and removed-database target selection issues. A fresh closure reviewer approved the final fixes and confirmed legacy CLI plan assertions were preserved.

A final independent storage review approved two additional corrections. Helm now filters disabled child CRDs on a separately loaded chart, preserving the SDK's single dependency pass and deployed-value fallback. Shared S3 credentials use reference-specific Secret names so an old ready projection cannot satisfy a replacement. `make test-platform-sdk` passed with real child-resource and CRD readbacks. `make test-platform-s3` passed with real OpenBao and External Secrets, different replacement keys, a missing replacement reaching the Secret wait, and the original projection unchanged. Restoring the old fixed Secret naming through a temporary Go overlay caused the rendering regression to fail as expected.

## Limits

This development cluster has no CSI snapshot driver or VolumeSnapshotClass. Preferred snapshots report unsupported and required mode rejects creation. Snapshot manifests, retention, and restore logic were reviewed and rendering tests passed, but a successful CSI snapshot recovery was not exercised here. S3 recovery and the weekly restore check were exercised on the real cluster. The E2E validates a selected full backup; point-in-time selection uses a conservative verified WAL window and was not separately exercised end to end.

## Final gate

`make pre-commit` passed on the final code in 412.25 seconds. It includes formatting and vet, all Go unit tests, seven Python pod-history tests, the live controller suite, and the full server integration suite. The additional final managed PostgreSQL recovery rerun passed. Both Make commands ran outside the sandbox before commits. A final Kubernetes readback confirmed no managed PostgreSQL clusters remained after test cleanup.

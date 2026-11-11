# Database upgrade and disaster recovery fixture

`db-test` is one Go binary with separate SQLite and Postgres drivers. It writes
random test values, records numbered checkpoints, and verifies them across
restarts, database upgrades and restores. CI performs the managed database
operations through Borq; this tool supplies and checks their data.

## Keep expectations outside the failed system

The CI runner generates an expected-data manifest **before contacting the
database**. It contains a scenario ID, checkpoint number and random seed. The
same file reproduces the exact expected numbers and binary values on any version
of the checker. The database never supplies its own expected seed.

Store these manifests in CI artifact storage or other durable storage outside
the database pod, its SQLite volume and the cluster being recovered. Require the
artifact upload to succeed before writing the checkpoint or starting a destructive
operation. A CI worker replacement must download the original artifact. Mount a
read-only copy into the workload or verification pod as needed; keep the retained
original outside that pod.

If SQLite goes offline and its pod dies, CI still has the expectation. Verification
waits until a recovered pod or another verifier can open the recovered volume.
For Postgres, the verifier reconnects to the recovered database. A missing or
corrupt expectation, unavailable database, wrong checkpoint or changed value
fails the command. The tool never treats an outage as success or reconstructs a
lost expectation from database contents.

`prepare` publishes complete files without overwriting an existing path. The
file is synced before success. This is a durable local write; uploading and
retaining it in the CI artifact service is still the CI workflow's responsibility.
Keep each checkpoint file immutable and associate it with its backup/recovery
point. Before/after report JSON and container logs are optional evidence, not the
source of expected values.

## Prepare and seed

On the CI runner, which needs no live database for this command:

```sh
make build-db-test
mkdir -p recovery-evidence
dist/db-test prepare --manifest recovery-evidence/checkpoint-1.json \
  --dataset ci-run-123 --checkpoint 1
# Upload checkpoint-1.json to durable CI artifact storage and require success.
```

After provisioning a disposable database, use that retained file:

```sh
export DB_TEST_DRIVER=sqlite
export SQLITE_DATABASE_PATH=/tmp/probe.sqlite
export DB_TEST_MANIFEST="$PWD/recovery-evidence/checkpoint-1.json"
dist/db-test seed
dist/db-test check
dist/db-test serve
```

For Postgres, use the same commands with `DB_TEST_DRIVER=postgres` and
`DATABASE_URL` from the managed database environment group. `--driver` and
`--database` can override the connection, and `--manifest` overrides
`DB_TEST_MANIFEST`. The driver must be selected explicitly. An optional `.env`
is loaded at startup; existing environment variables take precedence. Prefer
environment variables for connection credentials.

`seed` is the explicit initialization step. It creates both fixture tables and
128 rows in one transaction when absent, verifies an existing fixture, and
refuses partial or damaged data. `serve` (also the default command) and `check`
only verify existing data. They cannot initialize a missing database. This
prevents a replacement pod from hiding loss by generating the same values again.
`seed --require-existing` additionally forbids initialization for that command.

The fixed fixture schema stores text, signed integers, NULL versus empty text,
and binary bytes including NUL. Every checkpoint has fresh random numeric and
binary values derived from its externally generated 256-bit seed. Successful
one-shot commands report `driver`, `database_version`, `fixture_version`,
`dataset`, `checkpoint`, `samples`, `rows` and `checksum`. The three sample numbers
are actual verified database values you can compare between versions; SHA-256
fingerprints all row values. Database upgrades preserve the fixture schema.

## Advance and detect stale data

Before a later operation, prepare and retain another expectation with the same
scenario ID, the next checkpoint number and a new random seed:

```sh
dist/db-test prepare --manifest recovery-evidence/checkpoint-2.json \
  --dataset ci-run-123 --checkpoint 2
# Upload checkpoint-2.json to artifact storage and require success.
dist/db-test advance --manifest recovery-evidence/checkpoint-1.json \
  --next-manifest recovery-evidence/checkpoint-2.json

dist/db-test check --manifest recovery-evidence/checkpoint-2.json
```

`advance` first checks every current value against checkpoint 1. It then writes
checkpoint 2's random values and checkpoint marker atomically. It refuses damaged
data, missing rows, skipped checkpoints, different scenario IDs and reused seeds.
A retry after an interrupted response verifies an already committed checkpoint 2
without rewriting it. Keep both immutable expectation files through this operation.

An older checkpoint-1 restore fails when checked with checkpoint 2's manifest,
even within the same CI run. When deliberately restoring the checkpoint-1 backup,
use checkpoint 1's original manifest instead. To test point-in-time recovery,
retain manifests before and after the selected recovery point and check the one
that corresponds to the intended committed state.

A running `serve` pins its expected manifest on startup. After advancing, restart
it with `DB_TEST_MANIFEST` pointing to the new checkpoint. It checks immediately
and then logs comparisons every ten seconds. Initial verification failures exit
nonzero; failures after startup are logged while the process remains available
for exec. `--log-interval` changes the frequency and `--timeout` bounds each
operation (default 30 seconds). SIGINT/SIGTERM stop the process.

## CI upgrade and recovery sequence

1. Generate a random expectation on the CI runner and retain it outside the target
   cluster. Seed once and verify that checkpoint.
2. If the scenario needs newer writes, retain the next expectation, run `advance`,
   then verify the new checkpoint. Record which checkpoint each backup contains.
3. Perform the selected database restart, engine upgrade, failover or recovery.
4. Restore/download the original expected manifest from CI artifact storage. Run
   `check` against the recovered target before allowing any initialization there.
5. Require a successful check and retain its database version, sample numbers and
   checksum as evidence. Remove disposable scenario resources after verification.

The same sequence covers SQLite volume restarts, embedded-driver upgrades,
Litestream snapshot/WAL restores and offsite space recovery, plus managed
Postgres restarts/failover, version upgrades, base-backup/WAL recovery and supported
snapshot/offsite recovery. Controller completion and the selected recovery point
remain explicit CI assertions. Continuous writes are not generated automatically;
`advance` supplies controlled committed writes for those scenarios.

SQLite's engine is embedded in this binary's driver. Testing a version upgrade
means rebuilding the image with the new SQLite driver and checking the same file.
Upgrading Litestream alone does not upgrade SQLite. Postgres's engine is the
managed server and is upgraded separately. After a Postgres restore, use the
restored database's own credentials. See [SQLite operations](sqlite-applications.md),
[managed Postgres](managed-addons.md) and [offsite recovery](offsite-backups.md).
Automated managed-addon scenario wiring remains a separate integration step.

## Container and exec

```sh
make build-db-test-image
# The expectation has already been retained outside the target cluster.
docker volume create db-test-sqlite
docker create --name sqlite-probe \
  --mount type=volume,src=db-test-sqlite,dst=/var/lib/borq/sqlite \
  --mount "type=bind,src=$PWD/recovery-evidence,dst=/expected,readonly" \
  -e DB_TEST_DRIVER=sqlite -e DB_TEST_MANIFEST=/expected/checkpoint-1.json \
  borq-db-test:local serve
# Initialize once, sharing the new container's volume and expectation mount.
docker run --rm --volumes-from sqlite-probe \
  -e DB_TEST_DRIVER=sqlite -e DB_TEST_MANIFEST=/expected/checkpoint-1.json \
  borq-db-test:local seed
docker start sqlite-probe
docker exec sqlite-probe db-test check
```

In Borq, run the initial `seed` as an explicit setup step and use `serve` for
ordinary or recovered startups. The expectation file is mounted read-only into
both. For an initialized workload:

```sh
kubectl exec POD -- db-test check
kubectl exec -it POD -- /bin/sh
```

The image runs as UID 10001, GID 2000 and includes `/bin/sh`, TLS trust roots and
both drivers. SQLite defaults to `/var/lib/borq/sqlite/app.sqlite`; keep that whole
directory on persistent storage. The manifest must come from separate retained
storage. The same image connects to managed Postgres with its connection
environment. No HTTP port is required.

## Verify changes

```sh
make test-db-test
BORQ_ADDON_TEST_POSTGRES_URL='postgresql://user:password@localhost:5432/disposable?sslmode=disable' \
  make test-db-test-postgres
```

Tests cover offline expectation generation and immutable publication, missing or
corrupt artifacts, exact random values across drivers, durable reopen, corruption
and wrong-checkpoint detection, atomic checkpoint writes and retries, concurrency,
read-only startup, logs and shutdown. SQLite recovery uses a real snapshot after
the source is removed and rejects that snapshot against the newer checkpoint.
The Postgres suite requires a disposable database with schema creation permission.

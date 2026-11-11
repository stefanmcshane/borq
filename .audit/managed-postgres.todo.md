# Managed Postgres implementation

- [x] Read the Principles section of the poteto-mode skill.
- [x] Phase A: Frame.
- [x] Phase B: Design the workflow.
- [x] Phase C: Run the loop.
- [x] Phase D: Keep the audit trail.
- [x] Phase E: Verify and hand back.

## Throughput checkpoint

- Blocking first steps. Inspect current worktree, preserve existing changes, settle shared configuration and operation contracts before writers start.
- Independent workstreams. Explore configuration and managed Postgres lifecycle independently. Implement isolated backend, configuration, and transport/UI slices once contracts are agreed.
- Shared mutable state. Each writer gets its own worktree. The lead integrates completed patches sequentially. Only the lead regenerates the shared OpenAPI client after merging contracts.
- Smallest safe decomposition. One owner per behavior. Do not split rendering, reconciliation, and recovery into competing state owners.

## Implementation workflow

1. Settle typed management, S3 reference, managed Postgres, archive, and durable task contracts. Compare the two source-grounded ownership candidates independently.
2. Implement shared configuration/S3 and Postgres lifecycle in isolated worktrees. Integrate only completed units; retain one SQLite lifetime owner.
3. Wire the shared API contract, MCP exposure, CLI commands, and settings/addon UI. Verify focused behavior using Make targets.
4. Complete real backup discovery, restore-check jobs, snapshot capability/retention, runtime monitoring, and restart/rotation handling.
5. Author the public YAML seeded recovery E2E last. Run real create/seed/backup/catalogue/restore/equality/cleanup against explicit Borq test kubeconfigs.
6. Run independent review and product-surface checks. Run the full required gates outside the sandbox before any commit.

Done means the real public CLI lifecycle produces equal source/restored schema and per-table data hashes, both clusters clean up, management S3 eligibility and credential handling are enforced, configured jobs and monitoring read actual cluster state, all focused and required full gates pass, and independent review has no unresolved correctness findings.

Scope is three backend/platform slices and three transport/UI surfaces, followed by recovery acceptance. High rigor is warranted because restore correctness and archive ownership protect user data. Existing development storage has no CSI snapshot support; unsupported/required behavior can be proved there, while positive snapshot proof needs a CSI-capable test environment.

Baseline evidence: `make test-apps ARGS='-run TestCustomChart'` and `make test-apps ARGS='-run TestAddon'` passed at 190906e. No database feature implementation or recovery proof existed at baseline.

## Required outcome

- [x] One management YAML model embeds apps and managed/custom addons under orgs and spaces; standalone YAML uses the same types. No top-level version or profile.
- [x] One S3 configuration, credentials in OpenBao, development RustFS prefill, production external S3 eligibility and explicit org managed-addon enablement.
- [x] Management apply creates and updates component chart pins and inline values, orgs, spaces, applications and addons without implicit deletion.
- [x] Managed Postgres defaults to one instance, 1Gi storage, continuous WAL, immediate/daily full backups, 30-day recovery, hourly supported snapshots retained 24h.
- [x] Latest compatible stable Barman plugin is verified and pinned; real controller integration.
- [x] Durable create/apply/delete/restore operations, scoped S3 catalogue, environment retrieval, preserved archives, credential rotation and interruption recovery.
- [x] CLI borq addons managed postgres and root alias am; borq addons custom and root alias ac.
- [x] Useful UI/API/MCP/CLI logs and CPU, memory, disk, connection and query activity metrics without requiring Grafana.
- [x] Configurable weekly restore-check CronJobs with bounded resources, overlap prevention, real restored SQL validation and reliable cleanup.
- [x] Build the complete seeded-data E2E only after feature implementation. Create via small YAML, retrieve env through CLI, seed, trigger real full backup, list, restore new cluster, compare canonical row/table hashes plus schema/sequences/post-restore checks, always tear down both.
- [x] Full managed-addon recovery test and make pre-commit outside sandbox before commits. Focused Make checks during iteration.
- [x] Independent review, real UI/CLI exercise, requirement-by-requirement completion audit.

## Design exploration

- [x] Ground.
- [x] Sketch two structural candidates.
- [x] Cross-judge, select, and integrate useful ideas.
- [x] Implement.
- [x] Scrap only if evidence invalidates the chosen contracts. skip: runtime evidence supported the chosen contracts.

## Delivery policy

- No commit before required full checks. Existing unrelated work must remain intact.
- User explicitly selected gpt-6.1-sol with high reasoning for all subagents, overriding model-diversity defaults.
- Full recovery E2E is intentionally authored last, following the user's explicit sequence.

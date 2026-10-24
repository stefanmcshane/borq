# Testing
Always run tests using the make commands unless specified otherwise.
If any go files are changed during the turn, the following make commands should run outside the sandbox and succeed before a turn can end:
- make test-unit
- make test-integration-server
- make fmt

When `make dev` is running, a borq pod will run in the cluster with its traffic stolen by mirrord to a locally running service controlled by air, running on port 27833. All requests at localhost:7833 will route through the cluster to the local running service by design.

## Test ownership
- Give each behavior one primary test owner. Keep another layer when it covers a distinct transport, storage, security, or lifecycle risk; carry unique assertions into the keeper before removing duplicates.
- Keep model defaults and merge rules in application model tests, concrete manifests in rendering tests, live resource readback in inspection tests, and durable state and queued operations in manager tests. CLI tests own flags, profile selection, YAML, output, and exit behavior.
- Prove controller success through real Kubernetes integration tests. Fake clusters must not fabricate certificate issuance or other controller results and claim to verify that lifecycle.
- A persistence assertion must exercise the real write, close, reopen, and read path. Negative cases must reach their named guard and check the resulting error, rather than pass on an earlier unrelated failure.
- Remove production helpers or injection hooks whose only remaining callers are deleted tests. Preserve independent migration, credential-selection, protocol, default, and ordering contracts.

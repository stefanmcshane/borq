# Testing
Always run tests using the make commands unless specified otherwise.
- During iteration, run focused fast checks for the changed behavior, such as `make test-ui` or `make test-apps ARGS='-run TestName'`. Run `make fmt` for Go changes.
- Do not run the full cluster suite after every small change or at the end of every turn. Narrow real-cluster checks are appropriate when needed to prove a changed Kubernetes lifecycle behavior or when explicitly requested.
- Before creating a commit, run `make pre-commit` outside the sandbox and require it to succeed. This runs formatting, all unit tests, and `make test-slow` (the full server cluster suite).
- `make test-slow` is the explicit gate for slow cluster tests. Run additional subsystem acceptance suites when the changes require them; do not add them to routine UI or copy-edit checks.

When `make dev` is running, a borq pod will run in the cluster with its traffic stolen by mirrord to a locally running service controlled by air, running on port 27833. All requests at localhost:7833 will route through the cluster to the local running service by design.

## Test ownership
- Give each behavior one primary test owner. Keep another layer when it covers a distinct transport, storage, security, or lifecycle risk; carry unique assertions into the keeper before removing duplicates.
- Keep model defaults and merge rules in application model tests, concrete manifests in rendering tests, live resource readback in inspection tests, and durable state and queued operations in manager tests. CLI tests own flags, profile selection, YAML, output, and exit behavior.
- Prove controller success through real Kubernetes integration tests. Fake clusters must not fabricate certificate issuance or other controller results and claim to verify that lifecycle.
- A persistence assertion must exercise the real write, close, reopen, and read path. Negative cases must reach their named guard and check the resulting error, rather than pass on an earlier unrelated failure.
- Remove production helpers or injection hooks whose only remaining callers are deleted tests. Preserve independent migration, credential-selection, protocol, default, and ordering contracts.

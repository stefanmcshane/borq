Implement this testing structure.

We need two separate integration test flows, plus unit tests.

First, keep normal unit tests for pure code: validation, config parsing, manifest generation, Kubernetes object building, helper functions, and server business logic where it can be tested without a real cluster.

Second, keep a server integration test for active development.

The server integration test should:

1. Target a running borq server URL.
2. Default to `http://localhost:7833` when no URL is provided.
3. Support URL override with `BORQ_INTEGRATION_SERVER_URL`.
4. Assert live HTTP behavior for core endpoints (`/healthz`, `/`).
5. Avoid cluster bootstrap/image build work.

This should be runnable from:

make test-integration-server

Tilt may be the normal local runtime for this server test, but the test should work with any running borq server at the configured URL.

Third, keep a separate bootstrap integration test. This tests the CLI bootstrap path only. It should prove that a fresh machine/cluster can be bootstrapped correctly.

The bootstrap test should:

1. Create a fresh kind cluster.
2. Run the real CLI bootstrap command.
3. Confirm namespace, PVC, RBAC, Deployment, Service, and required config are created.
4. Confirm the server pod becomes ready.
5. Confirm the server health endpoint works.
6. Confirm basic persistence/state works if the server depends on storage.
7. Clean up the cluster unless a keep-cluster flag is set.

This should be runnable separately:

make test-integration-bootstrap

Image behavior for bootstrap tests:

- Default image is `ghcr.io/stefanmcshane/borq:latest`.
- Override image with `BORQ_INTEGRATION_BOOTSTRAP_IMAGE` when validating a specific tag or digest (including RC artifacts).

Expected commands:

make test-unit
make test-integration-server
make test-integration-bootstrap
make promote-release

The intended gates are:

Before commit / PR:
  make test-unit
  make test-integration-server
Before release:
  build/push candidate image
  BORQ_INTEGRATION_BOOTSTRAP_IMAGE=<candidate-tag-or-digest> make test-integration-bootstrap
  promote the same digest if it passes

Key rule:

Bootstrap tests check that installation works.
Server integration tests check that a running server behaves correctly.

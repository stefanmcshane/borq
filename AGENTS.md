# Testing
Always run tests using the make commands unless specified otherwise.
If any go files are changed during the turn, the following make commands should run outside the sandbox and succeed before a turn can end:
- make test-unit
- make test-integration-server
- make fmt

When `make dev` is running, a borq pod will run in the cluster with its traffic stolen by mirrord to a locally running service controlled by air, running on port 27833. All requests at localhost:7833 will route through the cluster to the local running service by design.
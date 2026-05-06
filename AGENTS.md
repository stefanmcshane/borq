# Testing
Always run tests using the make commands before ending a turn unless specified otherwise.
If any go files are changed during the turn, the following make commands should run outside the sandbox and succeed before a turn can end:
- make test-unit
- make test-integration-local
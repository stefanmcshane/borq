---
name: go-code-standards
description: Standard Go conventions for go/golang code. Use this skill to architect the code, discover where code should be in the repo, reason about project structure, CLI design, testing, and pre-commit workflow. Use when writing, reviewing, or maintaining any Go code, even if they do not mention Go code or Go standards.
---

- keep implementations simple. Do not add extra complexity unless it is needed. [Code simplifier](https://github.com/anthropics/claude-plugins-official/blob/main/plugins/code-simplifier/agents/code-simplifier.md?plain=1) has more detail on this
- never use pointers unless necessary. If a pointer is deemed necessary, the use of it should be documented in a comment.
- performance is never an excuse to use pointers unless explicitly told otherwise.
- never use init functions or globally defined variables unless necessary. if a global is necessary, it MUST have a meaningful comment explaining why package-global scope is required, even when unexported.
- do not create interfaces until they have at least 3 usecases. You can design with the idea of interfaces in future
- read environment variables once on startup and hold them in memory. Do not read from the environment ad hoc after this.
- use a dedicated config struct with `env` tags so each runtime field and its environment variable name are in one grepable place.
- load `.env` files at startup (for local development) before reading config, then only pass typed config through dependency injection.
- environment variable names, `.env` loading, and env parsing are boundary concerns. They belong in `cmd` (or `api` runtime boundary packages), not in `foundations`.
- unexport by default. We can later export functions if needed. Do not use an internal/ folder
- when naming functions, never use more than one capital letter at a time. `testHTTPSon` could be `test https on` or `test http son`. Using `testHttpSon` is clearer.
- prefer descriptive names over short names
- consider package names when naming. `virtualmachine.Create()` or `vm.CreateVirtualMachine()` is better than `virtualmachine.CreateVirtualMachine()`.  
- "default left" - avoid indenting where possible. The happy path of code should be able to be followed without being nested
- where possible, aim for readability by having simple functions which explain the flow of the code by using a top level function to encapsulate behaviour. Someone reading the code should be able to infer what happens within branches of a function with ease, and complicated logic 
```go
func provisionVmWithSoftware() err {
		vm, err := ensureVMIsCreated()
		err := ensureGitIsInstalled()
		err := ensurePythonIsInstalled()
		return nil
}
```
- for all 3rd parties which require connection, have an environment variable for enablement. Default to disabled. If it is enabled, check the presence of the required environment variables at runtime. If the env vars are present, attempt a connection. If a connection fails, shut down the commands. This runtime check/connection logic belongs in `api` startup, not in `cmd`. An example would be DB_ENABLED and DB_CONNECTION_URL.
- shared types packages should be avoided at all costs unless explicitly stated in this skill, or asked for by the user. Copying code around 3 times is acceptable. We do not need to "D.R.Y" (dont repeat yourself) to the extreme cases unless there are more than 3 references to the exact same code. Only then should we consider sharing the code, but thats not a guarantee
- use open telemetry by default unless told otherwise. 
- all exported functions, variables, and struct fields MUST have useful comments which explain the intention of the code in the correct golang format
- follow `accept interfaces, return structs`.
- do not expose protocol/domain payload builders as `string`/`[]byte` APIs unless the function is an explicit I/O boundary (HTTP/file/CLI output). Prefer typed request/response structs.
- for XML/JSON protocols, keep typed message models in the owning foundations package and marshal/unmarshal only at transport boundaries.
- Do not use named returns.
- Be explicit. Pass concrete values. Add seams only when the code has earned them.
- Prefer boring, direct construction. Do not add factories, constructor callbacks, provider functions, registries, or dependency-injection plumbing just to make code look flexible. Prefer function signatures like this when there are multiple inputs:
```go
func doSomething(ctx context.Context, inp doSomethingInput) (doSomethingOutput, error)
```

Construct the thing directly where it is used. The follow example is considered good code:
```go
type NewServerInput struct {
	Config Config
	Log    logger.Logger
}
func NewServer(ctx context.Context, inp NewServerInput) (Server, error) {
	kubernetesConnection, err := kubernetes.NewConnection(inp.Config.Kubernetes, inp.Log)
	if err != nil {
		return Server{}, fmt.Errorf("create kubernetes connection: %w", err)
	}
	server := Server{
		Config: inp.Config,
		Log:    inp.Log,
		HealthChecks: []HealthCheck{
			newKubernetesAuthHealthCheck(kubernetesConnection, inp.Config.Namespace),
		},
	}
	return server, nil
}
```
Do not hide simple construction behind callbacks or factories. The following example is considered bad code when not explictly required:
```go
type kubernetesConnectionFactory func(kubernetes.Config, logger.Logger) (kubernetes.Connection, error)
type healthCheckFactory func(kubernetes.Connection, string) HealthCheck
func NewServer(config Config, log logger.Logger) (Server, error) {
	return newServer(
		config,
		log,
		func(config kubernetes.Config, log logger.Logger) (kubernetes.Connection, error) {
			return kubernetes.NewConnection(config, log)
		},
		newKubernetesAuthHealthCheck,
	)
}
```
This is worse because the reader has to chase callbacks to understand what is being created. It adds indirection without buying anything useful.

- You may use a factory only when there is a real runtime choice between multiple implementations.
```go
type NewStorageClientInput struct {
	Provider  string
	S3Config  s3.Config
	GCSConfig gcs.Config
}
func NewStorageClient(ctx context.Context, inp newStorageClientInput) (StorageClient, error) {
	switch inp.Provider {
	case "s3":
		return s3.NewClient(ctx, inp.S3Config)
	case "gcs":
		return gcs.NewClient(ctx, inp.GCSConfig)
	default:
		return nil, fmt.Errorf("unknown storage provider %q", inp.Provider)
	}
}
```
Do not use factories for ordinary construction.

## Testing
- use matryer/is for testing unless told otherwise.
newly added code should include tests that keep coverage at or above 75% for that new code. Do not test code that just to hit this number. Write meaningful tests. Leeway can be given to avoid writing pointless code to satisfy the metric
- tests must not depend on another test's outputs (or execution order). If multiple tests need shared generated fixtures, create them in shared helpers or `TestMain` for that package/suite.
- test data should default to package-local `testdata` directories.
- generated test artifacts should default to ephemeral temp directories and be cleaned up after the suite unless an explicit env var opts into keeping them.
- if "examples" are only used by tests or local fixtures, move them under the owning package `testdata` instead of using a top-level `examples` folder.
- Companies House API mock/server OpenAPI specs live in `foundations/companieshouseapi/testdata/openapi`.
- XML Gateway monthly accounts fixtures and XSD fixture data live in `foundations/xmlgw/testdata`.
- if a file is test-only and used by automated tests, it belongs in package-local `testdata`, not in `zarf`.
- if an XSD is needed as test fixture data, store it in `foundations/xmlgw/testdata/xsd`. If an XSD is kept as archival/reference artifact not loaded by tests/runtime, it may live under `zarf`.
- every test should have a godoc comment when explains what the test does, and why it needs to be there. This should be keep up to date.

## Folder structures
```
  api
  cmd
  business
  foundations
  zarf
```
- cmd contains everything needed for starting the application such as reading environment variables, ensuring stdin/stdout/stderr is in place
- cmd will do a presence check for environment variables but will never attempt to use them to connect to 3rd party services or databases.
- cmd should not run liveness checks (database ping, third-party health probes, network handshakes). It only loads/parses config and does presence/shape checks.
- cmd may optionally emit non-fatal startup warnings when service-specific env vars are present but the corresponding `*_ENABLED` flag is false (for example `DB_URL` present while `DB_ENABLED=false`).
- cmd calls out to the api layer for runtimes. cmd has almost no exported function and is never called from other packages
- all applications should use cmd, but can have different cmds. If we bundled a local cli and server, we may have `cmd/my-app` and `api/my-app/server`, `api/my-app/cli` 
- "foundations" layer is for repositories and wrapping 3rd party APIs/SDKs in local types. These should have interfaces by default which are named as verbs. If we have a 3rd party SDK for mangaging virtual machines, the interface could be called VirtualMachineManager. Each interface should have a NewXXX() function where XXX is the interface name .i.e VirtualMachineManager has NewVirtualMachineManager().
- foundations layers may contain commonly used packages such as logger or telemetry setups which allow for basic overrides.
- foundations layers should use functional options/variadic options with validation where possible.
- foundations must not own env-specific config. No `.env` loading, no `os.Getenv`, and no env-var-name constants in foundations packages.
- foundations layers should be self contained and not leak any 3rd party types. Foundations layer should not contain business logic or abstractions. It should use terminology that is familiar to a user of the 3rd party service or database. If we wrapped the AWS EC2 SDK, we would use createEC2Instance to refer to the instance, we would not abstract this into createVM
- foundations packages should have a file of the same name which should serve as the entry point when browsing. This would contain the NewXXX() for example
- foundations level packages should only log at a debug level unless specified otherwise
- "business" layer contains the business-specific logic and workflows. All terminology should be business-specific terminology that any person within the company could understand the general idea of the flows. Any non-technical person should be able to read the exported business level function and understand approximately how the code works.
- business layers contain service level abstractions. These often become interfaces over time, but we should not prematurely create interfaces for business level abstractions.
- api is the entry point for everything. This handles starting up and connecting to all required services through dependency injection and must always "ask for" the variables and config it needs. 
- api should be started with a `NewXXX()` function which should test that all conditions for runtime are met, and fail early otherwise.
- api startup owns runtime dependency checks and connection setup for enabled services.
- env-backed config structs with `env` tags should live in `api/config` or the relevant `cmd/<app>/config` package, never in `foundations`.
- api should abstract away any protocol-specific logic before calling out to the business layer. This means that the business layer should have no idea if it was called from a HTTP server, gRPC server, or through a local command line
- api should export only its `NewXXX()` functions and its request/response models. When creating a HTTP server for example, `api/http` may contain a server package, a client package, and a shared types package.

### Zarf rules
- zarf is a holder around the service: non-runtime assets that support the repo but are not part of runtime package imports.
- zarf is for support material such as archived plans, design notes, migration notes, operational templates, and implementation spikes that are not imported by runtime code.
- zarf can contain domain-specific tooling folders such as `zarf/ixbrl` when they are support tooling or archival work and not part of main runtime package boundaries.
- zarf should contain project planning context in `zarf/plans`.
- Go runtime packages in `api`, `business`, `foundations`, and `cmd` must not import from `zarf`.
- If a file is a runnable example for users, it should live near the owning package and not in `zarf`.

## CLI conventions (local)

  - Keep `api/cli/root.go` small: root command creation, root-level persistent CLI flags, and subcommand registration only.
  - Never use package-level mutable globals for config, flags, or loggers.
  - Put each top-level command group in its own file under `api/cli` (for example `company_command.go`,
  `officer_command.go`, `file_command.go`, `docs_command.go`).
  - `RunE` should parse args/flags, call one execution function, and return errors. Avoid embedding workflow logic inline.

  - Output formatting is an `api/cli` concern.
  - Business/foundations return typed data and errors; they do not write to stdout/stderr and do not choose output mode.
  - `api/cli` must not introduce pass-through wrappers that only convert typed domain/protocol payloads into raw strings/bytes for later layers.
  - `--output` decision (`cli` vs `json`) is made in `api/cli`.
  - For thin pass-through reads, `api/cli` may call `foundations` directly.
  - Create `business` services only when there is real business workflow (policy/rules/orchestration shared across transports).
  - Do not create placeholder business interfaces for simple pass-through commands.
  - Use OpenTelemetry for logs, traces, and metrics.
  - Command result data is written to `stdout` only.
  - Operational telemetry (including logs) is written to `stderr` via OTel logging exporter and may also be exported remotely.
  - Include correlation fields in telemetry (`trace_id`, `span_id`, command name, request id where applicable).
  - Default visible telemetry level is `info` and `error`.
  - `--verbose` enables `debug` telemetry.
  - Structure telemetry fields (wide, keyed, non-interpolated), include error/non-error status consistently, and redact secrets (API keys, auth codes, tokens).

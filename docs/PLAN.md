# Plan

This is the working plan for building Forgeline. It is written so a fresh Claude Code
session (cloud or local) can pick it up with no other context. Update it as steps finish.

## Start here

Open this repository in Claude Code and paste:

> Read CLAUDE.md and docs/PLAN.md. Do the first unfinished step in the M1 list. Work in small
> commits, run `make ci` before each push, update the status in docs/PLAN.md and
> docs/ROADMAP.md, and stop at the end of the step so I can review. Tell me what you
> verified and what you did not.

## Where things stand

- M0 foundation: done. Repo, Apache-2.0 license, docs, Go skeleton, CI with a pinned checkout.
- M1 step 1: done. `internal/pipeline` has the model, `Validate`, and a generated JSON Schema
  with a drift test. `forgeline schema` prints it.
- M1 step 2: done. `pipeline.Load` and `LoadFile` read YAML, reject unknown fields, and return
  `*ValidationError` unchanged. Tests and `testdata/` cover valid, syntax error, unknown field,
  empty, and multi-problem files.
- M1 step 3: done. `pipeline.Plan` validates, detects cycles with the path in the error, and
  returns parallelizable layers plus a dependents map.
- M1 step 4: done, except the real-container test has not run here (no Docker daemon in the
  dev sandbox). `internal/executor` has the `Executor` interface, `Docker` and `Fake`;
  `internal/scheduler` runs a graph with bounded parallelism and skips dependents of failures.
- M1 step 5: done. `docs/design/002-spec-extensions.md` defines `inputs`, the cache key, and
  reserved `permissions`. Only `steps[].inputs` is in the types, schema and validation.
- M1 step 6: done in code and tests with a fake executor. NOT yet run against real Docker:
  run `examples/hello` twice on a machine with Docker before treating M1 as proven.
- Everything else is not started.

## Decisions already made

| Decision | Choice | Where |
| --- | --- | --- |
| Name, license, visibility | Forgeline, Apache-2.0, public | README, LICENSE |
| Spec shape | Steps with `needs` form a DAG. No fixed stages | `internal/pipeline` |
| Execution engine | Own minimal executor behind an interface. Dagger later as an optional step type | `docs/design/001-execution-engine.md` |
| First container runtime | Call the `docker` CLI through `os/exec` behind the interface, no Docker SDK yet | this plan |
| YAML library | `sigs.k8s.io/yaml` so the existing `json` tags drive YAML too | this plan |

Change any of these by writing a short design note first.

## M1 steps

M1 exit criterion: `forgeline run pipeline.yaml` runs a sample pipeline on a laptop and skips
steps whose inputs have not changed.

### Step 2: YAML loader (done)

- Add `sigs.k8s.io/yaml`. Check its license and note it in the commit message.
- In `internal/pipeline`, add `Load(r io.Reader) (*Pipeline, error)` and
  `LoadFile(path string) (*Pipeline, error)`. Reject unknown fields. Call `Validate` and return
  its `*ValidationError` unchanged.
- Add `testdata/` pipelines: one valid, and invalid ones covering syntax errors, an unknown
  field, and several validation problems at once.
- Acceptance: YAML syntax errors say where they are. Validation errors keep field paths.
  Tests cover valid files, each invalid case, and an empty file.
- Known limit: converting YAML to JSON loses line numbers for validation errors. Decision:
  acceptable for now. Syntax errors keep line numbers and validation errors keep field paths.
  Revisit if users find paths hard to map back to the file.

### Step 3: DAG planner (done)

- Add `Plan(p *Pipeline) (*Graph, error)` that builds the dependency graph.
- Detect cycles and name the path in the error, for example `cycle: a -> b -> c -> a`.
- Produce layers: each layer holds steps whose dependencies are all in earlier layers, so a
  scheduler can run a layer in parallel. Order inside a layer is deterministic (declaration order).
- Acceptance: tests for a linear chain, a diamond, independent steps, a cycle, a self-cycle
  caught earlier by `Validate`, and a forward reference.

### Step 4: executor and scheduler (done; Docker test unrun)

- Define in `internal/exec` (or similar):
  `type Executor interface { Run(ctx context.Context, s StepRun, logs io.Writer) (Result, error) }`.
  `StepRun` carries image, command or script, env, workdir, timeout, and the workspace path.
- `DockerExecutor`: `docker run --rm` through `os/exec`. Mount the workspace at `/workspace`
  and use it as the default workdir. Apply the step timeout through the context. Return the exit code.
- `FakeExecutor` for tests. The scheduler must be fully testable without Docker.
- Scheduler: run the graph layer by layer with bounded parallelism. When a step fails, skip every
  step that needs it, run unrelated steps to completion, and report the full outcome.
- Acceptance: scheduler tests use the fake executor for success, failure, skipped dependents,
  timeout, cancellation, and parallelism limits. One test marked to skip when Docker is absent
  runs a real container.

### Step 5: design note 002, spec extensions (done)

Do this before step 6, because caching needs declared inputs and the trust model needs
permission fields in the spec.

- Write `docs/design/002-spec-extensions.md` covering:
  - `inputs`: file globs that make up a step's cache key.
  - `outputs` or artifacts, only if needed for the sample pipeline.
  - Reserved `permissions` fields for M3: secrets the step may read, network access,
    and whether it may receive untrusted input. Say clearly what is reserved and not enforced yet.
  - Cache key definition: image reference, command or script, env, workdir, and a hash of the input files.
- Add only the fields step 6 needs to the types, with schema regeneration and validation tests.
- Acceptance: the note states what is implemented now versus reserved.

### Step 6: `forgeline run` with caching (done; real Docker run unverified)

- `forgeline run <file>` loads, validates, plans, and runs. Print one status line per step
  (ran, cached, failed, skipped). Exit non-zero on any failure.
- Cache: a local directory (for example `.forgeline/cache`) keyed as defined in note 002. A step
  whose key matches a previous success is reported as cached and not run again.
- Add `examples/hello/pipeline.yaml` with at least three steps, one of them dependent on two others.
- Acceptance: running the example twice shows the second run served from cache. Changing an
  input file re-runs only the affected steps. Document the example in the README.

When step 6 is done, mark M1 done in `docs/ROADMAP.md`.

## M2 steps (drafted, not started)

M2 exit criterion (from the roadmap): a single `forgeline server` binary backed by Postgres
accepts runs over a REST API, keeps run history, streams live logs, runs a pipeline for a
GitHub push or pull request event and reports commit status, and shows a DAG run view in a
browser.

Before step 1 starts, M1 should be proven once on a machine with Docker (see step 6 above).
Each step below ends with a green `make ci`; steps that touch trust or interfaces start with a
design note.

### Step 7: design note 003, control plane (done)

- Write `docs/design/003-control-plane.md` covering: the run and step state machines
  (queued, running, succeeded, failed, canceled; steps also cached and skipped), the data
  model, the REST resources, how logs are stored and streamed, and how a runner is attached.
- Decide and record: Postgres driver (candidate `pgx`, MIT), migration approach (embedded SQL
  files applied at start, no framework), HTTP stack (standard library `net/http`), auth for M2
  (a static bearer token from config; real identity is later).
- State what M2 does not do: no multi-tenant isolation, no sandbox beyond container defaults,
  no secrets management. Those are M3.
- Acceptance: the note names every table and endpoint step 8 to 12 will add.
- Done: `docs/design/003-control-plane.md`. Still open there: upload format and size limits,
  retention. The GitHub credential for M2 is a fine-grained token; a GitHub App comes later.

### Step 8: run store (done)

- Add `internal/store` with a `Store` interface for runs, step results and log chunks, plus an
  in-memory implementation used by tests and a Postgres implementation.
- Migrations are embedded SQL applied in order and recorded in a schema table.
- Acceptance: one shared test suite runs against both implementations. The Postgres run is
  gated on an environment variable so `make ci` passes without a database; CI sets it up.
- Done: `internal/store` (Memory, Postgres, embedded migration `0001_init.sql`). The shared suite
  passed against a real local Postgres 16 with `-race`; CI is configured to run it against
  Postgres 18 but that CI run has not been seen yet. Postgres tests use
  `FORGELINE_TEST_DATABASE_URL`.

### Step 9: server, REST API and embedded runner (done; Docker path unrun)

- `forgeline server --listen :8080 --database-url ...` serves: submit a run (pipeline YAML
  plus a source directory or checkout reference), list runs, get a run with step states, get a
  step's logs, cancel a run.
- The server executes queued runs with the existing scheduler and cache, in-process, with
  bounded concurrency. Separate runner processes are out of scope until a need appears.
- Every state change is written to the store; a server restart marks interrupted runs failed
  with a clear reason instead of leaving them running.
- Acceptance: handler tests with the fake executor cover submit, status transitions, failure,
  cancel, auth rejection, and restart recovery. `forgeline submit <file> --server URL` works.
- Done: `internal/server`, `internal/client`, `forgeline server` and `forgeline submit`. Tests
  cover all of the above plus hostile source archives and oversized uploads. The built binary
  was also exercised against a real local Postgres 16 (auth, persistence, failure reporting).
  Not run: a real Docker-backed run through the server (no Docker daemon in the dev sandbox).

### Step 10: live logs (done)

- Stream a step's logs with Server-Sent Events, replaying stored chunks first so a late
  client sees the full log.
- Acceptance: a test connects mid-run and receives the earlier lines then new ones in order;
  a finished run's log is served in full.
- Done: SSE in `internal/server/stream.go`, `client.StreamLogs`, and `forgeline submit --wait`
  now follows logs live. Tests cover replay then follow, multi-line chunks, resume with
  `Last-Event-ID`, steps that never ran, errors, and client disconnect. Verified with a fake
  executor only; not seen against real Docker output.

### Step 11: GitHub webhook trigger and commit status (not started)

- Verify the webhook signature (HMAC SHA-256) before reading the payload, in constant time.
- On push and pull request events, fetch the repository at the commit, read the pipeline file,
  run it, and post commit status (pending, success, failure) through the GitHub API.
- Trust limit, stated in the code and docs: until M3, only repositories and branches on an
  allow list in the server config may trigger runs, and pull requests from forks are ignored.
  Running untrusted pull request code without the M3 model would contradict the project's own
  principles.
- Acceptance: tests cover a valid and an invalid signature, an event from a repository not on
  the allow list, a fork pull request being ignored, and status transitions against a fake
  GitHub API. Which GitHub credential type to use (GitHub App or fine-grained token) is decided
  in note 003.

### Step 12: web UI with DAG run view (done; built before step 11 because the maintainer wants a web view to try)

- Static HTML and JavaScript embedded with `go:embed`, no build toolchain: a run list, a run
  page that draws the step DAG coloured by status, and a live log pane.
- Accessibility basics: status is never colour only, keyboard navigable.
- Acceptance: a test serves the UI; a manual check is recorded in the commit message, with a
  screenshot if a browser is available.
- Done: `internal/server/ui/`. Go tests cover serving, security headers and no inline script or
  style. A scripted Chromium (Playwright) check against a temporary dev server with a fake
  executor passed: sign-in and wrong token, run list, DAG with 6 steps and 7 edges, failed and
  skipped marking, live log, a hostile `<img onerror>` in log output rendered as plain text
  with no element created and no dialog, keyboard step selection, cancel, sign-out, no CSP
  violations, and no horizontal scroll at phone width. That browser script was not committed
  (it needs Node and Playwright); the dev server used was deleted. Not checked: dark mode
  visually, screen readers, browsers other than Chromium, real Docker output.

### Step 13: hosted demo on AWS (not started)

- Needs a decision from the maintainer first: account, region, budget, and public or private.
- Smallest viable shape: one container image of the server, a small managed Postgres, TLS, and
  an auth token. Infrastructure as code in the repository; no credentials in it.
- Acceptance: a documented, repeatable deploy and teardown, with the monthly cost estimated
  from current AWS pricing and checked against the budget before anything is created.

When step 13 is done, mark M2 done in `docs/ROADMAP.md`.

## After M2 (outline only)

- **M3 trust and sandbox model:** the differentiator. Write the threat model first, then
  enforce permissions, then write tests that try to break them.

Details for M3 and later are intentionally not written yet. Plan them when M2 is done.

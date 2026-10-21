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

### Step 5: design note 002, spec extensions (not started)

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

### Step 6: `forgeline run` with caching (not started)

- `forgeline run <file>` loads, validates, plans, and runs. Print one status line per step
  (ran, cached, failed, skipped). Exit non-zero on any failure.
- Cache: a local directory (for example `.forgeline/cache`) keyed as defined in note 002. A step
  whose key matches a previous success is reported as cached and not run again.
- Add `examples/hello/pipeline.yaml` with at least three steps, one of them dependent on two others.
- Acceptance: running the example twice shows the second run served from cache. Changing an
  input file re-runs only the affected steps. Document the example in the README.

When step 6 is done, mark M1 done in `docs/ROADMAP.md`.

## After M1 (outline only)

- **M2 control plane and UI:** single-binary server, Postgres, REST API, run history, live logs,
  GitHub webhook trigger with commit status, DAG run view. This is where AWS infrastructure
  first matters, for a hosted demo.
- **M3 trust and sandbox model:** the differentiator. Write the threat model first, then
  enforce permissions, then write tests that try to break them.

Details for M2 and later are intentionally not written yet. Plan them when M1 is done.

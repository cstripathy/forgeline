# 003: Control plane for milestone M2

Status: **Proposed.** Date: 2026-10-21. Nothing here is implemented. It fixes the shape that
M2 steps 8 to 12 build, so those steps do not each reinvent it.

## Scope

One binary, `forgeline server`, that accepts pipeline runs over HTTP, executes them with the
existing scheduler and cache, stores history in Postgres, streams logs, triggers from GitHub,
and serves a small web UI. Runs execute **in the server process** in M2. Separate runner
processes and Kubernetes execution stay behind the `Executor` interface for later.

Not in M2: multi-tenant isolation, per-step permissions, secrets management, and any sandbox
beyond container defaults. Those are M3. The server therefore must not be exposed to people who
are not trusted to run code on its host. This is stated in the README when the server lands.

## Decisions

| Decision | Choice | Why |
| --- | --- | --- |
| HTTP stack | Standard library `net/http` with Go 1.22+ pattern routing | No dependency; enough for a dozen routes |
| Postgres driver | `github.com/jackc/pgx/v5` (MIT) | Maintained, widely used; no ORM |
| Migrations | Embedded SQL files applied in order at start, recorded in `schema_migrations` | Single binary, no migration tool |
| Auth in M2 | One static bearer token from config, compared in constant time; GitHub webhooks use their own HMAC secret | Smallest thing that is not "open to the world" |
| Live logs | Server-Sent Events | One-way, works through proxies, no dependency |
| UI | Static HTML and JavaScript via `go:embed`, no build step | Keeps the single binary and the laptop-first rule |
| GitHub credential | Fine-grained personal access token with `statuses:write` and `contents:read` for M2; a GitHub App later | An App needs registration and key handling the maintainer has not set up; the token is replaceable behind one interface |
| Source for a run | The server checks out the commit into a per-run workspace directory | Steps get a clean workspace; the cache key still comes from input contents |

Dependencies need the licence check and a one-line reason in the commit that adds them.

## Run and step states

Run: `queued` -> `running` -> `succeeded` | `failed` | `canceled`.
Step: `pending` -> `running` -> `succeeded` | `failed` | `canceled`, or terminal `cached` or
`skipped` without running. These match the scheduler's statuses.

On start, the server marks any run left `running` as `failed` with reason
`server restarted`, and requeues nothing automatically. A queued run survives a restart.

## Data model

```
runs        id, pipeline_name, pipeline_yaml, status, reason, source_repo, source_ref,
            source_sha, trigger (api|github), created_at, started_at, finished_at
steps       run_id, step_id, status, exit_code, error, started_at, finished_at   (PK run_id, step_id)
log_chunks  run_id, step_id, seq, data, created_at                              (PK run_id, step_id, seq)
```

`pipeline_yaml` stores the exact text that ran, so history stays accurate if the file changes.
Log chunks are append-only and ordered by `seq`. Secrets are not stored in M2 because there
are none; `env` values appear in `pipeline_yaml`, so the docs say not to put secrets there.

## REST API (all under `/v1`, bearer token required except health and webhooks)

| Method and path | Purpose |
| --- | --- |
| `POST /v1/runs` | Submit a run: pipeline YAML plus either an uploaded source archive or a repo and ref. Returns `202` and the run |
| `GET /v1/runs` | List runs, newest first, with `limit` and a `before` cursor |
| `GET /v1/runs/{id}` | A run with its step states |
| `POST /v1/runs/{id}/cancel` | Request cancellation; idempotent |
| `GET /v1/runs/{id}/steps/{step}/logs` | The step's log; `Accept: text/event-stream` streams it live, replaying stored chunks first |
| `GET /healthz` | Liveness, no auth |
| `POST /webhooks/github` | GitHub events, authenticated by signature only |

Errors are JSON `{"error": "..."}` with the same "what is wrong and where" wording as the
rest of the project. Validation failures return the pipeline's field-path errors.

## GitHub webhook (step 11)

- Read the raw body, verify `X-Hub-Signature-256` with HMAC SHA-256 in constant time, and only
  then parse. Reject on a missing or wrong signature before doing any work.
- Handle `push` and `pull_request` (opened, synchronize, reopened) only.
- **Trust limit until M3:** the server config holds an allow list of `owner/repo` and
  branches. Events for anything else are acknowledged and ignored. Pull requests from forks
  are always ignored, because their code is untrusted and M2 has no sandbox to contain it.
- Post commit status `pending` at start and `success` or `failure` at the end, with a link to
  the run page. A failed status post is logged and does not change the run's result.

## Cancellation and limits

Cancel cancels the run's context, which already stops containers. The server limits concurrent
runs (config, default 2) and the pipeline's own parallelism (default 4). Log chunks are capped
per step (default 10 MiB) with a visible truncation marker, so one step cannot fill the disk.

## Decided while building step 9

- **Submission format:** `multipart/form-data` with a `pipeline` field (YAML, at most 1 MiB) and
  an optional `source` file, a gzip tar of the workspace. Default limit 64 MiB, compressed and
  unpacked, configurable.
- **Archive extraction is strict** because the archive is untrusted: only regular files and
  directories, relative names that stay inside the workspace, permissions normalised (setuid
  and similar dropped), no repeated names, bounded entry count and size. The archive is
  unpacked into scratch space before a run exists, so a bad archive creates no run.
- **No step cache in the server.** Each run gets a fresh workspace, so a cached step would be
  skipped without its output files existing for later steps. The cache only works where the
  workspace persists, as in `forgeline run`. Revisit with artifact storage.
- **Secrets stay out of argv.** The token and database URL come from `FORGELINE_TOKEN` and
  `FORGELINE_DATABASE_URL`, not flags. Tokens shorter than 16 characters are refused.
- **Without `FORGELINE_DATABASE_URL`** the server keeps runs in memory and says so. That is for
  trying it out, not for keeping history.
- **Default listen address is loopback.** The server speaks plain HTTP; TLS comes from a proxy.

## Log stream format (step 10)

`GET /v1/runs/{id}/steps/{step}/logs` with `Accept: text/event-stream`:

- Each stored chunk is one event: `id: <seq>` and `data: {"seq":N,"text":"..."}`. The text is
  JSON-encoded so newlines inside a chunk cannot break the framing.
- The stream replays stored chunks first, then follows new ones. It polls the store (default
  250 ms) instead of using in-process notification, so it also works with several server
  processes and costs at most one poll interval of latency.
- It ends with `event: end` and `data: {"status":"..."}` once the step is final and everything
  is delivered. Steps that never ran (skipped, canceled) end immediately with no chunks.
- The step's status is read before its chunks. Output is written before a step is marked
  final, so an empty read after a final status is a true end.
- A client that reconnects sends `Last-Event-ID: <seq>` and gets only later chunks.
- Idle streams send a `: keep-alive` comment every 15 s.

## What is not decided

- Retention: how long runs and logs are kept. M2 keeps everything.
- Workspace cleanup: run workspaces under the data dir are kept until retention is decided.
- How a hosted demo is protected from abuse beyond the bearer token; that belongs to step 13.

## What this note changes in the code

Nothing yet. Step 8 adds `internal/store`, step 9 the server and API, step 10 SSE, step 11 the
webhook, step 12 the UI.

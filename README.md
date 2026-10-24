# Forgeline

Forgeline is an open-source, self-hostable delivery platform where AI agents run safely.
It aims to combine typed pipelines, isolated execution, built-in security gates, and
progressive delivery (canary and blue-green) in one tool you can run yourself.

> **Status: pre-alpha.** Nothing here runs pipelines yet. This repository currently holds
> the project's direction, roadmap, and a minimal Go skeleton. See [docs/ROADMAP.md](docs/ROADMAP.md).

## Why

Research done in October 2026 pointed to four gaps:

- Most teams self-host their main CI/CD tool, and few plan to switch, so migration has to be easy.
- Teams want fewer tools: build, scan, and deploy in one place.
- AI agents inside CI have already leaked credentials through prompt injection, so agent
  execution needs isolation by design, not as an afterthought.
- Progressive delivery is usually stitched together by hand from separate tools.

## Principles

1. **Config as code, with real types.** Pipelines are typed objects with a published schema.
   A plain YAML form compiles to the same model.
2. **Local first.** The same pipeline runs on a laptop and in a cluster.
3. **Agents are untrusted by default.** A job never combines untrusted input, secrets, and
   external actions unless it explicitly declares and is granted each one.
4. **Small core, no in-process plugins.** Steps run as containers. Extensions use versioned,
   typed interfaces.
5. **Humans approve anything that changes production.** Every AI action is audited.

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/forgeline` | The command-line entry point |
| `internal/` | Private packages |
| `docs/ARCHITECTURE.md` | Intended architecture and open decisions |
| `docs/ROADMAP.md` | Milestones and their exit criteria |

## Build

```
make build   # builds ./bin/forgeline
make test
./bin/forgeline version
./bin/forgeline schema   # prints the pipeline JSON Schema
```

The schema is generated from the Go types in `internal/pipeline` and committed at
`schema/pipeline.v1alpha1.schema.json`. After changing the types, regenerate it with
`go test ./internal/pipeline -run TestSchemaMatchesCommittedFile -update`.

Requires Go 1.27 or newer.

## Try the example

`examples/hello/pipeline.yaml` has three steps: two independent ones feed a third that needs
both. It runs each step in a `docker run` container, so Docker must be running.

```
make build
./bin/forgeline run examples/hello/pipeline.yaml   # all steps: ran
./bin/forgeline run examples/hello/pipeline.yaml   # all steps: cached
echo "changed" > examples/hello/src/a.txt
./bin/forgeline run examples/hello/pipeline.yaml   # shout-a and combine: ran; shout-b: cached
```

The workspace is the pipeline file's directory, mounted at `/workspace`. A step is cached only
if it declares `inputs` and its last run with the same image, command, env, workdir and input
file contents succeeded. Cache records live in `.forgeline/cache`; `--no-cache` ignores them.
See [design note 002](docs/design/002-spec-extensions.md) for the exact key and its limits.
Containers run as the image's default user, so on Linux files they create (`out/`) may be
owned by root.

## Run everything on your laptop

You need Go 1.27+ and Docker. No cloud account, no database, no other setup.

```
make run-local     # builds, then starts the server on 127.0.0.1:8080 with in-memory history
```

It prints a link like `http://127.0.0.1:8080/#token=...`. Open it and you are signed in; the token
in the link is generated for that run only and is removed from the address bar. Then, in another
terminal, with the `export` line it printed:

```
./bin/forgeline submit --wait examples/hello/pipeline.yaml
```

Or skip the server entirely: `./bin/forgeline run examples/hello/pipeline.yaml` runs a pipeline
straight from the command line, with step caching.

To keep history across restarts, set `FORGELINE_DATABASE_URL=postgres://...` (and a fixed
`FORGELINE_TOKEN` of at least 16 characters, which is mandatory for any address other than
loopback). A throwaway Postgres: `docker run -d -p 5432:5432 -e POSTGRES_PASSWORD=dev postgres:18`.

## Run the server (pre-alpha)

```
export FORGELINE_TOKEN=$(head -c 24 /dev/urandom | base64 | tr -d '/+=')   # 16+ characters
export FORGELINE_DATABASE_URL=postgres://...   # optional; without it runs live in memory
./bin/forgeline server                           # listens on http://127.0.0.1:8080

# in another terminal, with the same FORGELINE_TOKEN:
./bin/forgeline submit --server http://127.0.0.1:8080 --wait examples/hello/pipeline.yaml
```

The API is in [design note 003](docs/design/003-control-plane.md): `POST /v1/runs`,
`GET /v1/runs`, `GET /v1/runs/{id}`, `POST /v1/runs/{id}/cancel`,
`GET /v1/runs/{id}/steps/{step}/logs`. Open `http://127.0.0.1:8080/` in a browser for the web UI: a run list, and a run page with the
step graph, live logs and a cancel button. It asks for the same token and keeps it for that
browser tab only.

**Warning:** there is no sandbox beyond container defaults until milestone M3. Anyone with the
token can run containers on the host, so keep the default loopback address or put the server
behind TLS and a network you trust. Server runs do not use the step cache.

Status: run on real Docker on 2026-10-21 (Amazon Linux 2023 on arm64, Docker, Alpine 3.24): the
example ran, a second run was served from cache, and changing `src/a.txt` re-ran only `shout-a`
and `combine`. The same example also passed through `forgeline server`. Not tried on macOS,
Windows, x86, Podman, or a rootless Docker setup.

## License

Apache License 2.0. See [LICENSE](LICENSE).

Copyright 2026 Chandra Sekhar Tripathy.

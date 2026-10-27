# 005: Threat model for the trust and sandbox work (M3)

Status: **Proposed.** Date: 2026-10-27. This is the written threat model that milestone M3
requires before any enforcement is built. It lists what is true today (measured, or marked as
not measured), what could go wrong, what we would change, and the test that tries to break
each change. Decisions that belong to the maintainer are collected at the end.

## What we protect

| Asset | Why it matters |
| --- | --- |
| The host running `forgeline server` | A step runs in a container, and the server talks to the Docker daemon, which is root on the host. Escaping a container, or controlling the server, is host compromise |
| Credentials reachable from the host | Cloud instance credentials, the database URL, the bearer token, the GitHub token and webhook secret in the server's environment |
| Secrets given to steps (not built yet) | Deploy keys, cloud credentials, registry logins |
| Source code in run workspaces | Private repositories are downloaded to the data directory |
| Integrity of results | A commit status that says `success` is what gates a merge |
| Availability | One run must not take the host down for others |

## Who and what is untrusted

| Input | Today it can come from |
| --- | --- |
| The pipeline file and its steps | Anyone holding the bearer token; anyone who can push to an allow-listed branch or open a same-repository pull request |
| Repository contents and pull request text | Contributors; later, content an AI agent reads |
| Uploaded and downloaded archives | Token holders; GitHub, for a repository we were told to trust |
| Step output (logs) | Whatever the step prints, which includes anything a hostile dependency prints |
| Webhook requests | Anyone on the network that can reach the server |
| Container images | Public registries, by mutable tag |
| AI agents (later) | Treated as untrusted by default, per the project's principles |

## Trust boundaries

1. **Network to server:** the REST API (bearer token) and the GitHub webhook (HMAC signature).
2. **Server to Docker daemon:** whoever controls what the server asks Docker to run controls the host.
3. **Step container to host and network:** the sandbox boundary this milestone is about.
4. **Step output to a browser:** log text is rendered in the web UI.
5. **Server to GitHub and cloud accounts:** the server holds tokens that act with real authority.
6. **Run to run:** all runs share one Docker host, one data directory and one database.

## What is true today

Measured on 2026-10-27 (Docker 29 on Ubuntu 24.04, WSL2) by running a default step container:

| Property | Measured |
| --- | --- |
| User inside the step | `root` (uid 0) |
| Capabilities | 14 of the default set (`CapEff 00000000a80425fb`) |
| `no_new_privileges` | off |
| Memory limit | none |
| Process limit | the host's (9163 here) |
| Outbound internet | reachable |
| Docker socket in the container | absent (the pipeline cannot request mounts; only the workspace is mounted) |
| With `--network none` | no interface but loopback; internet unreachable |
| With `--cap-drop ALL --security-opt no-new-privileges` | works for ordinary steps, including `apk add` |

From reading the code and the tests, not measured on a live deployment:

- Auth is one shared bearer token over plain HTTP; the server refuses short tokens and generates
  one only on loopback. There is no per-user identity, no rate limiting and no audit log.
- The GitHub webhook checks the HMAC signature before parsing, only acts for allow-listed
  repositories and branches, and ignores fork pull requests. The tests include two mutations of
  those checks that were confirmed to fail them.
- Upload and repository archives are extracted strictly (no escapes, no links for uploads,
  size and entry limits).
- Step logs are capped at 10 MiB each and rendered as text. An earlier session reported a
  scripted-browser check that a hostile `<img onerror>` in output stays inert; this session saw
  logs render as plain text but did not re-run that attack.
- The AWS demo template requires IMDSv2 with a hop limit of 1, which should stop containers
  from reaching instance credentials. Read from the template; not checked on the running
  instance.
- The Docker executor passes `env` as `-e KEY=VALUE` on the command line, so values are visible
  to every local user in the process list, and `docker inspect` shows them regardless.

## Threats

Status is one of **open** (nothing prevents it), **partly** (reduced, with the gap named), or
**handled** (a test exercises it).

| # | Threat | Status today | Planned change | A test that tries to break it |
| --- | --- | --- | --- | --- |
| T1 | A step sends source code or secrets to the internet, or calls internal services | **open**: network is on for every step | Per-step `permissions.network` of `none` or `egress`; default is an open decision | A `network: none` step tries TCP, DNS and the Docker bridge gateway; all must fail |
| T2 | A step abuses privileges inside or out of its container | **open**: root, 14 capabilities, `no_new_privileges` off | `no-new-privileges` for every step now; capability drop, PID and memory limits proposed (see decisions) | A step reads `NoNewPrivs`, tries to regain capabilities and to write outside the workspace |
| T3 | A secret leaks through the process list, `docker inspect`, or logs | **open**: env is in argv; no secrets feature yet | Secrets only for steps that declare them; never in argv; masked in logs | A step prints its secret and a second process lists processes; neither may show the value |
| T4 | **Untrusted input, secrets and an external action in one step** (the prompt-injection pattern) | **open**: there are no secrets yet, so no combination exists to forbid | A run started from untrusted input gets no secrets; a step must declare each of the three before it may combine them | An untrusted run asks for a secret and must be refused before any container starts |
| T5 | Hostile code in a pull request runs on the host | **partly**: fork PRs are ignored; same-repository PRs run with the writer's trust | Mark pull request runs untrusted for T4; keep the allow list | A fork PR payload is ignored (handled); a same-repo PR run gets no secrets (planned) |
| T6 | A forged or replayed webhook | **partly**: signature is checked (handled); a captured valid delivery can be replayed after a restart | Optional: bind deliveries to a time window | Tampered, unsigned and wrongly signed bodies are rejected (handled) |
| T7 | A hostile archive escapes the workspace | **handled** for upload (strict) and repository (links skipped) extraction | None | Traversal, links, oversize and bombs (handled) |
| T8 | A step plants a symlink in the workspace that the server later follows | **open, unmeasured**: the server reads workspace files only for the step cache, which the server does not use; cleanup does not follow links | Confirm with a test before adding any server-side workspace reads | A step creates a link to `/etc/passwd`; the server must never read through it |
| T9 | Script injection through step output in the UI | **handled** per an earlier session's browser test; not re-run here | Re-run the browser test in CI when a browser is available | `<img onerror>` in a log stays text |
| T10 | Stealing or guessing the bearer token | **partly**: 16+ characters, constant-time compare, no cookie so no CSRF; plain HTTP, one shared token, no lockout | TLS in front (documented), per-user tokens later | Wrong tokens are rejected (handled) |
| T11 | One run exhausts CPU, memory, disk or processes | **partly**: log and upload caps and run concurrency are limited (handled); per-step CPU, memory, PID and disk are not | Resource limits per step | A fork bomb and a memory hog must be stopped without hurting another run |
| T12 | A tampered or swapped container image | **open**: images are pulled by mutable tag | Digest pinning and signature checks belong to M4 | Out of scope here |
| T13 | An action nobody can reconstruct later | **open**: no audit log | Append-only audit records for runs, grants and approvals | Every grant produces a record, and the record cannot be edited through the API |
| T14 | Compromise of the server process is root on the host | **open by design**: it controls the Docker daemon | Run the server on a dedicated host or VM (the AWS demo does); rootless Docker or a stronger runtime later | Not testable in-process; recorded so it is not forgotten |

## Proposed slices

Each slice is a small step that ends with `make ci` and the breaking tests above. They are in
order of value for effort.

1. **Hardening and the network permission (T1, T2, T3 partly).** Add a minimal `permissions`
   block to a step with only `network: none | egress`, enforce `none` with Docker's `none`
   network, add `no-new-privileges` for every step, and stop putting env values in argv. Only
   the field that is enforced is added to the spec; the rest stay reserved as note 002 says.
2. **Secrets scoped to steps (T3).** `permissions.secrets` lists names. Values come from the
   server's configuration, never from the pipeline file. A step receives only what it lists;
   values are masked in stored logs and absent from argv.
3. **Trust of the run (T4, T5).** Each run is `trusted` or `untrusted` by how it started. An
   untrusted run is refused any secret. Combining secrets with network egress in one step
   requires both to be declared, and the combination is written to the audit log.
4. **Audit log (T13).** Append-only records for run starts, secret grants, approvals and
   cancels, with who or what caused them.
5. **Limits and capability drop (T2, T11).** Memory, PID and CPU limits and a capability drop,
   once the maintainer has decided how much compatibility to trade.

## Decisions needed from the maintainer

1. **Default network posture.** `egress` keeps every existing pipeline working but leaves T1 open
   unless a step opts in to `none`. `none` is safe by default but breaks any step that downloads
   dependencies, so those steps would need `network: egress` written out. The project's
   principle ("dangerous by default") points at `none`; the spec is alpha, so this is the cheapest
   time to change it. Recommendation: **`egress` for the first release of the field, with the
   example pipelines declaring their needs, and a decision to flip the default before M3 closes.**
2. **How much compatibility to trade for hardening.** `no-new-privileges` is proposed now.
   Dropping all capabilities worked in a quick `apk add` test but can break images that rely
   on capabilities (for example `chown` or `apt-get` as root). Recommendation: decide after
   slice 1, with a test run over a few common images.
3. **Are same-repository pull requests untrusted?** A collaborator's branch is code the
   maintainer did not review. Recommendation: yes, untrusted for secrets (T5).

## What this does not cover

Kubernetes execution, multi-tenant isolation between customers, image signing and scanning
(M4), and the AI agent governance model (M6). This note is about a single trusted operator
running possibly untrusted pipelines on one host.

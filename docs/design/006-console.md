# 006: The web console

Status: **Accepted by the maintainer, in progress.** Date: 2026-10-27.

## Question

The M2 web page can watch runs. People also need to set Forgeline up and operate it without
flags, curl or a shell: sign in, define pipelines, connect GitHub, manage secrets, see the state
of the platform and the cloud account under it, and change that cloud safely. What is built, in
what order, and what stops a UI that can do all of that from becoming the easiest way to hurt
the platform?

## Decisions

| Decision | Choice |
| --- | --- |
| Styling | Tailwind CSS v4.3.3 from its standalone CLI (no Node). The generated `app.css` is committed and embedded, so the binary stays self-contained and the CSP stays `style-src 'self'`. `make css` regenerates it; CI fails if it drifts |
| Front end | Plain ES modules, no framework and no build step beyond CSS. Untrusted text (pipeline and step names, logs, GitHub data) is always inserted as text |
| Persistence | One small document store (`internal/docstore`): `(kind, id) -> JSON`. Memory with an optional file for a laptop, Postgres for a server. Runs stay in the existing run store |
| People | Local accounts with PBKDF2-SHA256 (standard library) and three roles. The existing bearer token keeps working for the CLI and API and counts as an admin named `token` |
| Secrets | Encrypted at rest with AES-256-GCM. Write-only: the API lists names and metadata, never values |
| Cloud | Every change is a proposal that someone else approves, then executes, and is audited. No UI path changes the cloud directly |

## Roles

| | viewer | developer | admin |
| --- | --- | --- | --- |
| See runs, pipelines, infra | yes | yes | yes |
| Run, cancel, edit pipelines | no | yes | yes |
| Propose cloud changes | no | yes | yes |
| List secret names | no | yes | yes |
| Create or delete secrets, connect GitHub, manage users, approve cloud changes, read the audit log | no | no | yes |

A person who proposed a cloud change cannot approve it ("four eyes"). The bearer token counts as
one actor named `token`.

## Safety rules the console must keep

1. **Same trust model as before.** Runs started by GitHub still get no secrets (note 005,
   slice 3 decides trust levels). Runs started from the console by a signed-in developer or
   admin may use secrets, because that person could run any container on the host already.
2. **Secrets cannot be read back.** Not by the API, not in logs (masked), not in the audit log.
   The GitHub token and webhook secret live in the same vault under names containing a dot
   (`forgeline.github.token`), which can never be a valid step secret name, so a pipeline cannot
   request them.
3. **Every change is audited**: who, what, to which object, from where, and whether it was
   allowed. Failed logins and refused actions are recorded too. The log is append-only through
   the API.
4. **Cookies**: `HttpOnly`, `SameSite=Strict`, `Secure` behind TLS. A request that changes
   state and carries a cookie must also carry `X-Forgeline-Request: 1`, which a cross-site page
   cannot add without a CORS preflight, and the server allows none. Origin is checked too.
5. **First run**: the first account is created at `/setup` and needs the server's bearer token,
   so whoever can reach the port first cannot take the server over.
6. **Login throttling** per account and per address.
7. **Cloud changes** run through CloudFormation change sets so a reviewer sees the real diff
   before anything changes. Execution uses the server's own AWS credentials, so deploy the
   server with a role that can do only what you want the console to be able to do.

## What each page is

- **Overview**: every pipeline with its current status, recent history and success rate; a live
  list of runs; the platform's health at a glance.
- **Pipelines**: saved definitions with versions, a YAML editor with live validation and a graph
  preview, run and history.
- **Run**: the DAG coloured by status, step details and live logs (the M2 view, restyled).
- **Infrastructure**: the Forgeline server, its Docker host and the steps running on it, queue
  and capacity; the AWS account (identity, stacks, instances, spend) read through the AWS CLI;
  cloud change requests and their approval.
- **Integrations**: GitHub connection, allowed repositories and branches, webhook URL and last
  deliveries, a connection test.
- **Secrets**: names, who changed them, when; add, rotate, delete.
- **People and audit**: users, roles, sessions; the audit log with filters.

## Order of work

Each step is tested with Go tests and with a real browser against a running server, and the
screenshots are kept in `docs/console/`.

1. Storage, vault, accounts, sessions, roles, audit.
2. Tailwind shell, sign-in and setup, overview, run view.
3. Pipelines.
4. Secrets.
5. GitHub integration.
6. Infrastructure overview.
7. Cloud changes with approval.
8. Users and audit pages.

## Not in this note

Single sign-on (OIDC/SAML), per-project permissions, multi-tenant separation, and a secret
store other than the built-in vault. Each of those is a separate decision.

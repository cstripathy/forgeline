---
title: Endpoints
parent: API reference
nav_order: 1
---

# Endpoints

Role = the minimum role. A test (`internal/server/apidocs_test.go`) fails if a route is missing from this page or has a different role, so the lists stay true to the code. *public* needs no credentials.

## Health and sign-in

| Method and path | Role | Purpose |
| --- | --- | --- |
| `GET /healthz` | public | Liveness. |
| `GET /v1/auth/state` | public | Is setup needed, and who am I. |
| `POST /v1/auth/setup` | public (needs server token) | Create the first admin. Body: `token`, `username`, `name`, `password`. |
| `POST /v1/auth/login` | public | Body: `username`, `password`. Sets the session cookie. Throttled. |
| `POST /v1/auth/logout` | viewer | Ends the session. |
| `POST /v1/me/password` | viewer | Body: `current`, `new` (at least 12 characters). |

## Runs

| Method and path | Role | Purpose |
| --- | --- | --- |
| `POST /v1/runs` | developer | Submit a pipeline. `multipart/form-data`: field `pipeline` (YAML) and optional `source` (an archive of the files the steps read). |
| `GET /v1/runs` | viewer | List runs, newest first. |
| `GET /v1/runs/{id}` | viewer | One run with its steps and their status. |
| `POST /v1/runs/{id}/cancel` | developer | Stop a run. |
| `GET /v1/runs/{id}/steps/{step}/logs` | viewer | A step's output. Send `Accept: text/event-stream` to follow it live. Secrets appear as `***`. |

A run's `status` is one of `queued`, `running`, `succeeded`, `failed`, `canceled`. A step's is one of `pending`, `running`, `succeeded`, `cached`, `failed`, `skipped`, `canceled`.

## Pipelines

| Method and path | Role | Purpose |
| --- | --- | --- |
| `GET /v1/pipelines` | viewer | Saved pipelines with latest status. |
| `POST /v1/pipelines/validate` | viewer | Body: `yaml`. Returns problems with line numbers and the step graph. Saves nothing. |
| `POST /v1/pipelines` | developer | Save a new pipeline. Body: `yaml`, `description`. The name comes from the YAML. |
| `GET /v1/pipelines/{id}` | viewer | The pipeline and its versions. |
| `PUT /v1/pipelines/{id}` | developer | New version. Body: `yaml`, `description`, `base_version`. `409` if someone saved after `base_version`. |
| `POST /v1/pipelines/{id}/run` | developer | Run the latest version. May use secrets. |
| `POST /v1/pipelines/{id}/versions/{version}/restore` | developer | Make an old version the newest. |
| `DELETE /v1/pipelines/{id}` | admin | Delete the pipeline and its versions. |
| `GET /v1/overview` | viewer | Everything the Overview page shows. |

## Secrets

| Method and path | Role | Purpose |
| --- | --- | --- |
| `GET /v1/secrets` | developer | Names, descriptions, who changed them and when. Never values. |
| `PUT /v1/secrets/{name}` | admin | Body: `value` (at least 8 characters), `description`. Name matches `[A-Za-z_][A-Za-z0-9_]*`. |
| `DELETE /v1/secrets/{name}` | admin | Remove. |

## People and audit

| Method and path | Role | Purpose |
| --- | --- | --- |
| `GET /v1/users` | admin | List. |
| `POST /v1/users` | admin | Body: `username`, `name`, `password`, `role`. |
| `PATCH /v1/users/{id}` | admin | Any of `name`, `role`, `disabled`, `password`. The last admin cannot be removed or demoted. |
| `DELETE /v1/users/{id}` | admin | Remove. |
| `GET /v1/audit` | admin | The audit log, newest first. Filters by query string. |

## GitHub

| Method and path | Role | Purpose |
| --- | --- | --- |
| `GET /v1/integrations/github` | developer | Current settings (no secrets), last deliveries. |
| `PUT /v1/integrations/github` | admin | Body: `enabled`, `api_base`, `public_url`, `pipeline_file`, `rules` (`owner/repo` plus branches), `token` (write-only). |
| `DELETE /v1/integrations/github` | admin | Disconnect and delete stored keys. |
| `POST /v1/integrations/github/test` | admin | Check the connection. |
| `POST /v1/integrations/github/app/start` | admin | Begin creating the App. Body: `public_url`, `name`, `owner`, `api_base`. |
| `GET /v1/integrations/github/app/launch` | admin | The page that sends you to GitHub. |
| `GET /v1/integrations/github/app/callback` | public | GitHub returns here with `code` and `state`. Bound to the admin's setup by a one-time state. |
| `GET /v1/integrations/github/app/installations` | admin | Installations of the App. |
| `GET /v1/integrations/github/app/repos` | admin | Repositories of an installation. |
| `POST /v1/integrations/github/app/install` | admin | Body: `installation_id`, `rules`, `public_url`. |
| `POST /webhooks/github` | public (signed) | Where GitHub sends events. Checked with the webhook secret; a bad signature gets `401`. |

## AWS

| Method and path | Role | Purpose |
| --- | --- | --- |
| `GET /v1/integrations/aws` | developer | Settings and identity. |
| `PUT /v1/integrations/aws` | admin | Use a server profile. Body: `enabled`, `profile`, `region`. The name `forgeline-sso` is reserved. |
| `GET /v1/integrations/aws/mcp` | admin | Whether the AWS MCP Server is on, the tools it offers, and the proxy and endpoint used. |
| `PUT /v1/integrations/aws/mcp` | admin | Body: `enabled`. Turning it on starts the signing proxy and checks the server offers `aws___run_script`. |
| `POST /v1/integrations/aws/sso/start` | admin | Body: `start_url`, `sso_region`. Returns `login_id`, the `user_code` and the link to approve. |
| `POST /v1/integrations/aws/sso/poll` | admin | Body: `login_id`. Never blocks; returns `status`: `pending`, `expired`, `denied`, or `authorized` once approved. |
| `GET /v1/integrations/aws/sso/accounts` | admin | Accounts you may use. |
| `GET /v1/integrations/aws/sso/roles` | admin | Roles in an account (`?account_id=`). |
| `POST /v1/integrations/aws/sso/connect` | admin | Body: `account_id`, `account_name`, `role_name`, `region`. Tests the credentials before keeping them. |
| `GET /v1/integrations/aws/sso` | developer | Whether you are signed in, and until when. |
| `DELETE /v1/integrations/aws/sso` | admin | Sign out and delete the stored token. |
| `GET /v1/ai/workflows` | developer | Workflows: stages, steps, schedule, run-as role. |
| `GET /v1/ai/workflows/{id}` | developer | One workflow with every version. |
| `POST /v1/ai/workflows` | admin | Create a workflow (version 1): stages of agent steps, optional gate per stage, optional schedule, run-as viewer or developer. |
| `POST /v1/ai/workflows/{id}/versions` | admin | Save a new version; agent versions are pinned. |
| `POST /v1/ai/workflows/{id}/rollback` | admin | Copy an old version forward as the current one. |
| `POST /v1/ai/workflows/{id}/disable` | admin | Stop it running, including its schedule. |
| `POST /v1/ai/workflows/{id}/enable` | admin | Turn it back on. |
| `POST /v1/ai/workflows/{id}/run` | developer | Start a run now (one at a time per workflow). |
| `GET /v1/ai/workflow-runs` | developer | Recent runs (`?workflow=`). |
| `GET /v1/ai/workflow-runs/{id}` | developer | One run with each step's output. |
| `POST /v1/ai/workflow-runs/{id}/release` | admin | Let a run waiting at a gate start its next stage. |
| `POST /v1/ai/workflow-runs/{id}/cancel` | developer | Cancel a run (its starter, or an administrator). |
| `GET /v1/k8s/clusters` | viewer | Registered Kubernetes clusters (never their tokens). |
| `POST /v1/k8s/clusters` | admin | Register a cluster (`eks` through a connected AWS account, or `token`); proves it with a read before saving. |
| `DELETE /v1/k8s/clusters/{id}` | admin | Remove a cluster and its stored token. Changes nothing in the cluster. |
| `GET /v1/k8s/clusters/{id}/resources` | viewer | Nodes, workloads, pods, services, namespaces and warning events, read-only; `?refresh=1` skips the 60 s cache. |
| `GET /v1/guard/findings` | viewer | Guard findings (`?status=open|resolved`, `?account=`, `?severity=`) with severity counts and any accepted exception. |
| `GET /v1/guard/scans` | viewer | Recent scans and the automatic scan interval. |
| `POST /v1/guard/scan` | developer | Start a scan of every account (or `account`); read-only; one at a time. |
| `PUT /v1/guard/settings` | admin | Automatic scans every 6, 12, 24 or 168 hours, or 0 for off. |
| `POST /v1/guard/findings/{id}/accept` | admin | Accept a finding for 1 to 90 days with a reason; it reopens when that ends. |
| `DELETE /v1/guard/findings/{id}/accept` | admin | End an acceptance early. |
| `GET /v1/ai/tools` | developer | The tools an agent can be given, with the role each needs. |
| `GET /v1/ai/skills` | developer | Skills (name, description, current version). |
| `GET /v1/ai/skills/{id}` | developer | One skill with every version. |
| `POST /v1/ai/skills` | admin | Create a skill (version 1). |
| `POST /v1/ai/skills/{id}/versions` | admin | Save a new version; history is append-only and an unchanged save is refused. |
| `POST /v1/ai/skills/{id}/rollback` | admin | Make an old version current by copying it forward as a new version. |
| `DELETE /v1/ai/skills/{id}` | admin | Remove a skill no agent version uses. |
| `GET /v1/ai/agents` | developer | Agents with current version, tier and tools. |
| `GET /v1/ai/agents/{id}` | developer | One agent with every version. |
| `POST /v1/ai/agents` | admin | Create an agent (version 1): instructions, pinned skills, tool allowlist, tier T0 or T1. |
| `POST /v1/ai/agents/{id}/versions` | admin | Save a new version; skill pins are made exact. |
| `POST /v1/ai/agents/{id}/rollback` | admin | Copy an old version forward as the current one. |
| `POST /v1/ai/agents/{id}/disable` | admin | Turn an agent off (it cannot run or be listed). |
| `POST /v1/ai/agents/{id}/enable` | admin | Turn it back on. |
| `POST /v1/ai/agents/{id}/chat` | developer | Talk to an agent (`version` optional). It uses only its own tools, as the asker. |
| `GET /v1/aws/accounts` | viewer | The connected AWS accounts: the primary one and any added. |
| `POST /v1/aws/accounts` | admin | Add a read-only account by profile and region; proves it with `sts get-caller-identity`, refuses duplicates. |
| `DELETE /v1/aws/accounts/{id}` | admin | Remove an added account. Changes nothing in AWS. |
| `GET /v1/infra/aws` | viewer | Identity, stacks, instances. `?account=<id>` reads an added account. |
| `GET /v1/infra/aws/resources` | viewer | Every resource in the account by service (ECS, EC2, S3, Lambda, RDS and more), read-only; `?account=<id>` picks the account; `?refresh=1` skips the 60 s cache. |
| `GET /v1/infra/aws/cost` | developer | Spend for 30 days (each call is billed by AWS). |
| `GET /v1/infra/runtime` | viewer | The Forgeline server, Docker host, running steps, disk. |

## Cloud changes

| Method and path | Role | Purpose |
| --- | --- | --- |
| `GET /v1/cloud/catalog` | developer | Templates that can be deployed, and the instance actions. |
| `POST /v1/cloud/changes` | developer | Propose. Body: `kind` (`stack-deploy`, `stack-delete` or `instance-action`), `reason`, and `template`, `stack`, `params` or `instance_id`, `action` (`start`, `stop`, `reboot`). |
| `GET /v1/cloud/changes` | viewer | List. |
| `GET /v1/cloud/changes/{id}` | viewer | One change, including the preview of exactly what would happen. |
| `POST /v1/cloud/changes/{id}/approve` | admin | Body: `note`. Not allowed for the person who proposed it. Runs the change. |
| `POST /v1/cloud/changes/{id}/reject` | admin | Body: `note`. |
| `POST /v1/cloud/changes/{id}/cancel` | developer | Withdraw your own proposal. |

## GitHub Actions

Only repositories on the GitHub allow list. A workflow can only be started on a branch its rule allows.

| Method and path | Role | Purpose |
| --- | --- | --- |
| `GET /v1/actions/repos` | developer | The repositories you may use, and their branches. |
| `GET /v1/actions/workflows` | developer | `?repo=owner/name`. Workflows of the repository. |
| `GET /v1/actions/runs` | developer | `?repo=`, optional `workflow=` (number or `ci.yml`) and `status=`. Latest 30 runs. Each has `fork: true` when its code came from another repository. |
| `GET /v1/actions/runs/{id}` | developer | `?repo=`. The run and its jobs with steps. |
| `GET /v1/actions/jobs/{id}/logs` | developer | `?repo=`. The last 256 KB of the job's log as plain text. |
| `POST /v1/actions/dispatch` | developer | Body: `repo`, `workflow`, `ref` (an allowed branch), `inputs` (up to 25 strings). Needs `workflow_dispatch` in the workflow file. |
| `POST /v1/actions/runs/{id}/cancel` | developer | Body: `repo`. |
| `POST /v1/actions/runs/{id}/rerun` | developer | Body: `repo`, `failed_only`. |

## Assistant

| Method and path | Role | Purpose |
| --- | --- | --- |
| `GET /v1/assistant` | developer | Whether it is on, and which backend. |
| `PUT /v1/integrations/assistant` | admin | Body: `backend` (`off`, `api`, `claude-token`, `cli`), `model`, `api_base`, `api_key` and `claude_token` (both write-only; the token is stored as the secret `CLAUDE_CODE_OAUTH_TOKEN`). |
| `POST /v1/integrations/assistant/test` | admin | Ask it something small to check the setup. |
| `GET /v1/ai/proposals` | viewer | Changes the assistant drafted, newest first, with their status. |
| `GET /v1/ai/proposals/{id}` | viewer | One proposal: the exact script, the AWS calls it makes, risk, and (after approval) what ran. |
| `POST /v1/ai/proposals/{id}/approve` | admin | Body: `hash` (of the text you read), `note`. Not allowed for the person who proposed it. Applies the approved script through the AWS MCP Server and returns 202; poll the proposal. |
| `POST /v1/ai/proposals/{id}/reject` | admin | Rejects a waiting proposal. |
| `POST /v1/ai/proposals/{id}/cancel` | developer | The proposer or an administrator withdraws a waiting proposal. |
| `POST /v1/assistant/chat` | developer | Body: `message`, `history`, `context` (the page: run, pipeline, infra or overview). Returns `reply`, the read-only `lookups` it ran and any `proposals` it filed. 20 a minute per person. |

## The pipeline schema

`forgeline schema` prints the JSON Schema; the same file is in the repository at
[`schema/pipeline.v1alpha1.schema.json`](https://github.com/forgelinehq/forgeline/blob/main/schema/pipeline.v1alpha1.schema.json).

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
| `POST /v1/integrations/aws/sso/start` | admin | Body: `start_url`, `sso_region`. Returns `login_id`, the `user_code` and the link to approve. |
| `POST /v1/integrations/aws/sso/poll` | admin | Body: `login_id`. Never blocks; returns `status`: `pending`, `expired`, `denied`, or `authorized` once approved. |
| `GET /v1/integrations/aws/sso/accounts` | admin | Accounts you may use. |
| `GET /v1/integrations/aws/sso/roles` | admin | Roles in an account (`?account_id=`). |
| `POST /v1/integrations/aws/sso/connect` | admin | Body: `account_id`, `account_name`, `role_name`, `region`. Tests the credentials before keeping them. |
| `GET /v1/integrations/aws/sso` | developer | Whether you are signed in, and until when. |
| `DELETE /v1/integrations/aws/sso` | admin | Sign out and delete the stored token. |
| `GET /v1/infra/aws` | viewer | Identity, stacks, instances. |
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

## Assistant

| Method and path | Role | Purpose |
| --- | --- | --- |
| `GET /v1/assistant` | developer | Whether it is on, and which backend. |
| `PUT /v1/integrations/assistant` | admin | Body: `backend` (`off`, `api`, `claude-token`, `cli`), `model`, `api_base`, `api_key` and `claude_token` (both write-only; the token is stored as the secret `CLAUDE_CODE_OAUTH_TOKEN`). |
| `POST /v1/integrations/assistant/test` | admin | Ask it something small to check the setup. |
| `POST /v1/assistant/chat` | developer | Body: `message`, `history`, `context` (the page: run, pipeline, infra or overview). Returns `reply`. 20 a minute per person. |

## The pipeline schema

`forgeline schema` prints the JSON Schema; the same file is in the repository at
[`schema/pipeline.v1alpha1.schema.json`](https://github.com/forgelinehq/forgeline/blob/main/schema/pipeline.v1alpha1.schema.json).

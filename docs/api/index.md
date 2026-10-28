---
title: API reference
nav_order: 4
has_children: true
permalink: /api/
---

# API

Everything the console does goes through this REST API, so anything you can click you can script.
Bodies are JSON unless noted. Times are RFC 3339 UTC.

## Authentication

**Bearer token** (scripts, CI, the CLI). Send the server token. It counts as an admin named `token`.

```sh
export FL=http://127.0.0.1:8080 TOKEN=$(cat /tmp/fl-demo/.token)
curl -s -H "Authorization: Bearer $TOKEN" $FL/v1/runs
```

**Session cookie** (the console). `POST /v1/auth/login` sets `forgeline_session`
(`HttpOnly`, `SameSite=Strict`). A request that changes state and uses a cookie must also send
`X-Forgeline-Request: 1`. Bearer requests do not need it.

```sh
curl -s -c jar -H 'Content-Type: application/json' -H 'X-Forgeline-Request: 1' \
  -d '{"username":"admin","password":"..."}' $FL/v1/auth/login
```

## Roles

Each endpoint needs a minimum role: **viewer** < **developer** < **admin**. See the
[reference]({{ '/api/reference/' | relative_url }}) for each one. A role that is too low gets `403`.

## Errors

Failures return a status code and `{"error": "what went wrong, in words you can act on"}`.

| Code | Meaning |
| --- | --- |
| 400 | The body or a value is not acceptable. The message says which. |
| 401 | No or wrong credentials. |
| 403 | Signed in, but the role is too low. |
| 404 | No such object. |
| 409 | The request conflicts with the current state (for example a pipeline saved by someone else since you opened it). |
| 429 | Too many attempts or questions. |

## Quick tour

```sh
H="Authorization: Bearer $TOKEN"

# Check a pipeline without saving it
curl -s -H "$H" -H 'Content-Type: application/json' $FL/v1/pipelines/validate \
  -d '{"yaml":"version: v1alpha1\nname: demo\nsteps:\n  - id: a\n    image: alpine:3.24\n    run: echo hi\n"}'

# Save it
curl -s -H "$H" -H 'Content-Type: application/json' $FL/v1/pipelines \
  -d '{"yaml":"version: v1alpha1\nname: demo\nsteps:\n  - id: a\n    image: alpine:3.24\n    run: echo hi\n","description":"first"}'

# Run it and read a step's log
curl -s -X POST -H "$H" $FL/v1/pipelines/demo/run
curl -s -H "$H" $FL/v1/runs/RUN_ID/steps/a/logs

# Add a secret (write-only)
curl -s -X PUT -H "$H" -H 'Content-Type: application/json' $FL/v1/secrets/REGISTRY_TOKEN \
  -d '{"value":"s3cret-value","description":"registry push"}'
```

{: .note }
The exact fields of responses can change while Forgeline is pre-alpha. Request bodies below are the ones the server reads today.

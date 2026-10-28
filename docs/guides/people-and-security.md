---
title: People and security
parent: Guides
nav_order: 7
---

# People and security

## Roles

| | viewer | developer | admin |
| --- | :-: | :-: | :-: |
| See runs, pipelines, infrastructure | ✓ | ✓ | ✓ |
| Run, cancel, edit pipelines | | ✓ | ✓ |
| Propose cloud changes, ask the assistant | | ✓ | ✓ |
| List secret names | | ✓ | ✓ |
| Create or delete secrets, connect GitHub and AWS, manage users, approve changes, read the audit log | | | ✓ |

The bearer token counts as an admin named `token`. Whoever proposes a cloud change cannot approve it.

![People]({{ '/console/11-people.png' | relative_url }})

## What protects you

- Passwords use PBKDF2-SHA256, at least 12 characters, with login throttling per account and per address.
- Cookies are `HttpOnly` and `SameSite=Strict`. Requests that change something must send `X-Forgeline-Request: 1`.
- Secrets are encrypted with AES-256-GCM, cannot be read back, and are masked in logs.
- Steps get only the secrets and network they ask for. Runs started by GitHub get no secrets.
- Every change, refusal and failed login goes to the audit log.

See [design note 006](https://github.com/forgelinehq/forgeline/blob/main/docs/design/006-console.md) for the rules and the known limits.

---
title: Status
nav_order: 5
---

# Status

Pre-alpha. Built and tested, with the race detector, real Docker and real browsers:
pipelines and runs, the console, secrets, roles and audit, GitHub triggers, agent steps, the
assistant, AWS sign-in, the GitHub App.

**Checked on real services:** AWS sign-in, an approved deploy and delete of a stack on a real account, a GitHub App created and installed on github.com, and the assistant on a real Claude Code login token.

**Not checked against the real service:** a real GitHub webhook delivery and commit status; the assistant's API-key mode against the real
Anthropic API; TLS behind a real proxy; more than one server sharing a database; single sign-on (not built).

Roadmap and plan: [ROADMAP](https://github.com/cstripathy/forgeline/blob/main/docs/ROADMAP.md), [PLAN](https://github.com/cstripathy/forgeline/blob/main/docs/PLAN.md), [ARCHITECTURE](https://github.com/cstripathy/forgeline/blob/main/docs/ARCHITECTURE.md).

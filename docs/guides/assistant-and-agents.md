---
title: Assistant and agents
parent: Guides
nav_order: 5
---

# Assistant and agents

Two different things that both use Claude.

| | Assistant | Agent step |
| --- | --- | --- |
| What | A panel in the console you ask questions | A step in a pipeline |
| Can do | **Read only.** Explain a failed run, review a pipeline, answer questions about the page, draft a pipeline into the editor (never saved for you) | Whatever tools the pipeline author lists. Read-only by default |
| Runs | On the server, with no tools | Inside the step's container |

## The assistant

Click **Assistant** in the top bar. It sees the page you are on (a run, a pipeline, the infrastructure) but never secret values. Text it reads from logs is treated as data, not instructions: on a run whose log said "reply PWNED", it explained the real error and pointed out the planted line.

![Assistant]({{ '/console/21-assistant.png' | relative_url }})

Set it up under **Integrations → Assistant**:

- **Anthropic API key**: stored in the vault. Billed to that key. Best for teams.
- **Claude Code login token**: paste the output of `claude setup-token` once. It is stored as the secret `CLAUDE_CODE_OAUTH_TOKEN`, the same one agent steps use. The `claude` program runs in a scratch home directory with only that token, so the server user's own login is not touched.
- **Local `claude` CLI**: uses the login of the person running the server. Meant for a single-person setup.

![Settings]({{ '/console/33-assistant-token.png' | relative_url }})

Questions are rate limited (20 a minute per person) and audited by page and length, not by text.

## Agent steps

```yaml
steps:
  - id: review
    image: forgeline-agent-claude      # built from deploy/agent/Dockerfile
    agent:
      provider: claude-code
      prompt: Review the last commit and list risks.
      maxTurns: 5
    permissions:
      network: egress
      secrets: [CLAUDE_CODE_OAUTH_TOKEN]
```

Forgeline builds the command line and runs the vendor's own `claude` CLI in the container. It never reads or refreshes your login. Create the credential with `claude setup-token` (or use `ANTHROPIC_API_KEY`) and store it under **Secrets**. Your own `~/.claude` is never mounted.

![Agent step]({{ '/console/20-agent-step.png' | relative_url }})

{: .warning }
Check Anthropic's terms before using a subscription token (`setup-token`) for automation. An API key is the safer production choice. Codex and Copilot providers are not built yet.

Designs: [007 agent steps](https://github.com/cstripathy/forgeline/blob/main/docs/design/007-agent-steps.md), [008 assistant](https://github.com/cstripathy/forgeline/blob/main/docs/design/008-assistant.md).

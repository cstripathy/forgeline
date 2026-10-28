# 007: Agent steps (Claude Code first)

Status: **Built for Claude Code**, login unverified (see the end). Date: 2026-10-28.

## Question

A pipeline step should be able to hand work to a coding agent: review a diff, explain a failure,
propose a fix. People already pay for Claude, Codex or Copilot. How does a step use those logins
without Forgeline holding or refreshing vendor tokens, and without an agent being able to do more
than the pipeline author allowed?

## What other tools do (researched 2026-10-28)

OpenClaw and Hermes do one of three things. They run the vendor's own command line as a
subprocess (`claude -p`), which keeps the login inside the vendor's program; or they read the
vendor's credentials file and call the API themselves; or they run the vendor's OAuth login
(PKCE for Codex, device flow for Copilot) and store the tokens. Only the first leaves token
handling to the vendor. Claude subscription tokens used by a third-party HTTP client are
reported to be enforced against, and OpenClaw's own docs call API keys the safer production path.
Those reports were not verified against Anthropic's terms. Check them before relying on a
subscription login.

## Decisions

| Decision | Choice |
| --- | --- |
| How the agent runs | The vendor's own command line, inside the step's container. Forgeline never reads, copies or refreshes a vendor token |
| Credential | A vault secret named in `permissions.secrets`: `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`) or `ANTHROPIC_API_KEY`. Masked in logs, never cached, withheld from GitHub-triggered runs, exactly like any secret (note 005, slice 2) |
| Your own `~/.claude` | Never mounted into a container |
| Spec | `agent: {provider, prompt, model?, maxTurns?, tools?}`, an alternative to `run` and `command`. Exactly one of the three |
| Command line | Built by Forgeline, never a shell: `claude -p --output-format text --max-turns N --allowedTools=<list> [--model M] -- <prompt>`. The prompt follows `--`, so text starting with `-` or holding shell characters cannot change the command |
| Tools | Read-only by default (`Read, Grep, Glob`). Editing files or running commands must be listed. A headless agent cannot ask for approval, so anything not listed is refused |
| Validation | The provider must be known; the prompt is required; the step must request a credential for its provider; the network must not be `none` |
| Image | The author names it. `deploy/agent/Dockerfile` builds `forgeline-agent-claude` with a pinned Claude Code version and no credentials |

## Not built yet

- Codex (`codex` plus `auth.json` as a secret) and Copilot (its CLI plus a GitHub token). The
  `provider` enum and `AgentCredentials` map are the extension points.
- A console page that walks through the login. Today you run `claude setup-token` yourself and
  add the result under Secrets.
- A network allowlist limited to the vendor's hosts (needs note 005 slice 3 or later). Until then
  an agent step has full egress.
- Trust levels: a pull request from a fork must never reach an agent with a credential. GitHub
  runs get no secrets today, so they cannot run an agent step, which is the safe default.
- Spend limits (`--max-budget-usd`) and token usage in the console.

## Checked, and not checked

Checked: validation and the command line in Go tests (the credential rule was mutation-proved), the
step on real Docker with the real Claude Code binary, and the console path from the step builder
through the vault to the container. With a dummy token the agent reached Anthropic and answered
401, so the plumbing works end to end. Picture: [20-agent-step.png](../console/20-agent-step.png).

**Not checked:** a run with a genuine login, so no real agent output and no spend were observed.
Whether a `setup-token` login on a subscription is permitted for this use is not confirmed.

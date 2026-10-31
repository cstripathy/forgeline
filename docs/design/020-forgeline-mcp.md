# 020: Forgeline as an MCP server

Status: built.

External AI clients (Claude Code, other agents, IDEs) can use Forgeline over MCP. They get what the
console's assistant has, no more: read-only lookups, and proposals that a different administrator
approves. Nothing an MCP client does can change the outside world by itself.

## Who is calling

The shared server token means "administrator" and is never accepted here. A person creates a
**personal API token** on the Account page:

- It belongs to one user and has a role no higher than that user's, chosen at creation (viewer,
  developer or admin). The effective role is the lower of the token's and the user's *now*, so a
  demoted user loses the token's power at once. Disabling or deleting a person deletes their tokens,
  so enabling them again does not bring old tokens back.
- It is shown once; only its SHA-256 is stored. It expires (default 90 days, at most 365). A user
  has at most 10. The owner (or an administrator) can revoke it.
- It works on `/mcp` only. A cookie session does not work there (a browser would send it
  automatically, which invites cross-site and DNS-rebinding use), and a token cannot create tokens.

## Transport

Streamable HTTP, stateless: `POST /mcp` takes one JSON-RPC message and answers with JSON
(`initialize`, `ping`, `tools/list`, `tools/call`; notifications answer 202). `GET` answers 405 (no
server-initiated stream) and there is no session id. A request with an `Origin` that is not this
server is refused. `forgeline mcp` is a stdio proxy for clients that only speak stdio (it refuses a plain `http://`
server URL unless the host is this machine, since the token would cross the network in clear): it reads
messages on stdin, posts them to the server with the token from `FORGELINE_MCP_TOKEN`, and writes
the answers to stdout.

## Tools

Exactly the assistant's lookups that read: `list_runs`, `get_run`, `get_step_log`, `run_failure`,
`list_pipelines`, `get_pipeline`, `aws_accounts`, `aws_resources`, `aws_summary`, `k8s_clusters`,
`k8s_resources`, `guard_findings`, `environment_templates`, `list_agents`, and `search_audit` (admin).
Three propose-only tools file the usual four-eyes proposals: `propose_aws_change` (the assistant's
script proposal), `propose_cloud_change` (stack deploy, stack delete, instance action) and
`request_environment` (never created without a different administrator, even under self-service). Each tool keeps the role it has
in the console, so a viewer's token sees only the viewer tools.

Left out on purpose: `call_aws` and `aws_script` (raw cloud access, administrators only in the
console), `ask_agent` (spends model budget and runs tools), plugin tools, and anything that applies,
approves or reads a secret value.

## Safety

- Every `tools/call` is audited (`mcp.call`, with the caller, tool and a short argument summary),
  and so are refusals (`mcp.denied`) and token creation and revocation.
- Output is data. Credential formats are masked first and the text is then cut at 12 KB (the other way round, a credential on the cut would escape). Formats (AWS keys, GitHub and Slack tokens,
  Anthropic keys, private keys, live Stripe keys) become `[credential removed]` as a second line of
  defence, in answers, error messages and the audit trail; the lookups already never return secret values.
- Limits: 1 MiB request bodies, 60 calls a minute and 4 at once per token, 60 seconds per call.
- Proposals made this way are filed as the token's user and cannot be approved by that user, and
  each audit entry names the token that made the call.

## Not built

OAuth for MCP clients (tokens only), server-initiated streams and sessions, resources and prompts,
Kubernetes change proposals (held), and per-tool token scopes narrower than a role.

# 008: The assistant

Status: **Built.** Date: 2026-10-28. Pictures: [21](../console/21-assistant.png), [22](../console/22-assistant-settings.png).

## Question

People want to ask the console things: why did this run fail, what does this pipeline do, what is
running in AWS, draft me a pipeline. What may such an assistant see and do, and how does it reach
Claude, without becoming the easiest way to leak a secret or change the platform?

## Decisions

| Decision | Choice |
| --- | --- |
| What it can do | Answer in text. It has no write tools; since 2026-10-30 it can also run a few read-only lookups (see "Lookups" below). A pipeline it drafts is only text until a person opens it in the editor and saves it; a cloud change still needs a proposal and a second administrator |
| What it sees | Only what the console already shows the signed-in person, chosen by the page (a run with its pipeline, step states and the tail of each step's log; a pipeline; the AWS summary; the recent runs). Logs were masked when stored. It never gets secret values |
| Untrusted data | Everything gathered is placed inside `<untrusted_context>` and the system prompt says it is data, never instructions. A closing tag inside the data is escaped so it cannot end the block |
| Who | Developers and administrators ask; administrators configure. Twenty questions a minute per person |
| Backends | `api`: the Anthropic Messages API with a key in the vault (internal name `forgeline.assistant.api_key`, never usable by a pipeline). `cli`: the `claude` program on the server, run with `--tools ""`, one turn, no session file, an empty scratch directory and a clean environment (only HOME and PATH), so the server's own token and keys never reach it |
| `claude-token` backend | Added later. The same `claude` run as `cli`, but with the vault secret `CLAUDE_CODE_OAUTH_TOKEN` (the one agent steps use) as the only credential and a scratch `HOME`, so it does not depend on the server user's login and works on a server. Tested with a stub `claude` (token present, scratch home, no leak) and with the real program in the browser |
| Which to choose | `api` for any server with more than one person. `cli` uses the server user's own Claude login for everyone who can ask, so it is offered only as a single-person (laptop) mode, with a warning on the settings card |
| Audit | Every question is recorded with the page it was about and its length, not its text. Settings changes and tests are recorded |

## What was checked

Go tests (with the race detector): the request to the model carries the key, the model and the
run's log inside the untrusted block; a closing tag in a log cannot escape it; no tools are sent;
orphan history turns are dropped; the key appears in no endpoint, secret list or audit entry and
the secrets API refuses its name; a provider refusal is reported without the key; viewers are
refused, the limit returns 429; and the `cli` backend is started with no tools and without the
server's token in its environment. Each of the three safety rules was removed in turn and a test
failed.

In a real browser with the real `claude` on this machine: the settings card saved and tested the
`cli` backend; on the page of a failed run the assistant explained the missing file, and when the
log held a planted line telling the assistant to reply only with "PWNED" it reported that line as
suspicious and did not obey it; it drafted a pipeline that opened in the editor, valid and unsaved.

**Not checked:** the `api` backend against the real Anthropic API (no API key was available; a
stand-in server was used), several people using one server, and long conversations (history is
capped at twelve turns). Replies are shown as plain text: markdown such as `**bold**` is not
rendered.

## Lookups (added 2026-10-30, design note 013 item A2)

The assistant can fetch more of what the console shows instead of only what the page picked. It
asks by replying with exactly one fenced block (`forgeline-lookup`) holding a tool name and
arguments; the **server** runs it as the person who asked and gives the result back inside
`<lookup_result>` as data. This works with every backend because it does not use native tool
calling.

| Rule | Detail |
| --- | --- |
| Tools | `list_runs`, `get_run`, `get_step_log`, `list_pipelines`, `get_pipeline`, `aws_summary`, and `search_audit` (administrators only). None writes, applies, approves or reads a secret value |
| Role | Checked on the server for each call; the prompt lists only the tools the asker may use |
| Strict parsing | The whole reply must be one block. A block inside prose, or two blocks, is treated as an answer and nothing runs |
| Limits | At most 6 lookups a question, results cut to 12 KB, a closing tag inside a result is escaped |
| MCP lookups | With the AWS MCP Server on (note 016): `aws_script` (administrators, read-only scripts), documentation tools, and `propose_aws_change`, which only files a proposal. `call_aws` is off in that mode |
| `call_aws` | Administrators only; one read-only AWS CLI command through the console's AWS connection. Rules in design note 015 |
| Audit | Each lookup is recorded (`assistant.lookup`) and the chat entry counts them; the panel shows "looked up: ..." |

Checked with Go tests (race detector): lookups run server-side and come back as data, a planted
closing tag cannot escape, prose or double blocks do not run, unknown tools and tools above the
asker's role are refused, a runaway model is cut off, and no native `tools` are sent. Rules were
broken one at a time to confirm a test fails (role, escape, cap). Two parse rules (prefix, single
block) are also guarded by JSON parsing, so removing either alone changes nothing. Tried with the
real `claude` through the `claude-token` backend: it listed runs, found a failing run and read
its log.

With more than one AWS account connected (note 017) the assistant first calls `aws_accounts`, then passes an `account` id to `aws_summary` or `call_aws`. `call_aws` stays off for the primary account while the AWS MCP Server is on, but works for added accounts because the MCP server only serves the primary one.

New lookups (notes 017): `aws_resources` (the explorer's view of an account, shared with its cache), `list_agents` and `ask_agent`. While an agent runs, the lookup list is cut to the agent's own tools, `ask_agent` is never offered, and each lookup is still checked against the asking person's role.

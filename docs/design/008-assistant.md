# 008: The assistant

Status: **Built.** Date: 2026-10-28. Pictures: [21](../console/21-assistant.png), [22](../console/22-assistant-settings.png).

## Question

People want to ask the console things: why did this run fail, what does this pipeline do, what is
running in AWS, draft me a pipeline. What may such an assistant see and do, and how does it reach
Claude, without becoming the easiest way to leak a secret or change the platform?

## Decisions

| Decision | Choice |
| --- | --- |
| What it can do | Answer in text. It has **no tools**. A pipeline it drafts is only text until a person opens it in the editor and saves it; a cloud change still needs a proposal and a second administrator |
| What it sees | Only what the console already shows the signed-in person, chosen by the page (a run with its pipeline, step states and the tail of each step's log; a pipeline; the AWS summary; the recent runs). Logs were masked when stored. It never gets secret values |
| Untrusted data | Everything gathered is placed inside `<untrusted_context>` and the system prompt says it is data, never instructions. A closing tag inside the data is escaped so it cannot end the block |
| Who | Developers and administrators ask; administrators configure. Twenty questions a minute per person |
| Backends | `api`: the Anthropic Messages API with a key in the vault (internal name `forgeline.assistant.api_key`, never usable by a pipeline). `cli`: the `claude` program on the server, run with `--tools ""`, one turn, no session file, an empty scratch directory and a clean environment (only HOME and PATH), so the server's own token and keys never reach it |
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

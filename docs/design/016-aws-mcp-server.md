# 016 The AWS MCP Server: read lookups and approved changes

Status: **built** 2026-10-30. Pictures: [40](../console/40-mcp-settings.png), [41](../console/41-proposals-list.png), [42](../console/42-proposal-review.png), [43](../console/43-proposal-applied.png).
Replaces the plan in note 015 for the managed server; `call_aws` (the AWS CLI version) stays as the fallback when the MCP server is off.

## What the real server does (probed 2026-10-30 against the managed endpoint, read-only)

- Reached through AWS's signing proxy: `uvx mcp-proxy-for-aws@1.6.0 https://aws-mcp.us-east-1.api.aws/mcp --metadata AWS_REGION=<region>`, using the console's AWS profile. MCP over stdio, protocol `2025-06-18`.
- **`aws___call_aws` is not offered** on this endpoint, although the migration guide lists it. The tools it did offer: `aws___run_script`, `aws___get_presigned_url`, `aws___get_tasks`, `aws___list_regions`, `aws___get_regional_availability`, `aws___read_documentation`, `aws___search_documentation`, `aws___retrieve_skill`.
- `aws___run_script` takes Python (`code`) and calls AWS with `call_boto3(service_name=..., operation_name=..., params=...)`. It returns `return_value` and an `api_calls` list of what it called. A real `sts GetCallerIdentity` through it returned the account.
- The proxy's `--read-only` flag hides every tool not marked read-only, including `run_script`, so it cannot be used for API calls at all. The read-only rule therefore lives in Forgeline.

## Decisions

| Decision | Choice |
| --- | --- |
| Connection | A child process per call (`internal/mcp`): start the pinned proxy, handshake, one call, stop. No long-lived processes. The proxy command and endpoint come from the server's environment (`FORGELINE_AWS_MCP_PROXY`, `FORGELINE_AWS_MCP_ENDPOINT`), never from the console, so no administrator can make the server run an arbitrary program. Clean environment: only HOME, PATH, LANG and the SSO role's keys |
| Switch | Administrators turn it on under Infrastructure → AWS. Turning it on starts the proxy and requires `aws___run_script` to be offered; otherwise it stays off and says what was offered |
| Reads (assistant) | `aws_script` (administrators): the script is read first (`checkScript`), every `call_boto3` must name its service and operation as quoted text and be Describe, List or Get, with a never-list for secrets, credentials, data, logs and code. After the call, the `api_calls` the sandbox reports must be a subset of what the script declared, or the result is discarded |
| Docs (assistant) | `aws_search_documentation`, `aws_read_documentation`, `aws_retrieve_skill`, `aws_list_regions`, `aws_get_regional_availability` for developers and up, forwarding only each tool's own arguments. `aws___get_presigned_url` is not exposed |
| Writes | The assistant can only **file a proposal** (`propose_aws_change`). The script must contain at least one change and passes the same code rules. Not allowed to propose: Delete/Terminate/Remove/Deregister/Disable/Detach/Revoke/Purge/Destroy/Disassociate/Reset/Cancel/Release operations, and changes to `iam`, `organizations`, `sts`, `account`, `cloudtrail`, `guardduty`, `securityhub`, `config`, `ssm`, plus the services refused for reads |
| Approval | A different administrator approves, naming the sha256 of the text they read. The approval fails if the text, the AWS profile or region, or the MCP setting changed, if the proposal is older than 24 hours, or if another approved change is being applied. Developers can withdraw their own proposals |
| Apply | Forgeline runs exactly the approved script (never model output) through the proxy, in the background, and records the sandbox's `api_calls`. If the sandbox reports a call that was not in the approved script, the proposal is marked **failed** with that call named, because it cannot be undone from here |
| Audit | `ai.proposal.create/approve/reject/cancel/apply`, `integration.awsmcp.update`, and every script lookup with its tool name |

## Code rules for scripts (reads and changes)

Only these imports: asyncio, json, datetime, re, collections, itertools, math, statistics, typing.
No f-strings or `.format`, none of `eval exec compile open globals locals getattr setattr vars input
dir type super os sys subprocess socket importlib builtins pickle ctypes aws_mcp` or any `__name__`.
Comments and string contents are ignored when looking for names. `call_boto3` may only be called
directly (not stored, passed or aliased), and every occurrence must be a call with literal
service and operation. A region, if given, must be a real region name. Scripts are limited to 6000 characters.

## What was checked

Go tests with the race detector: the MCP client (list, call, error result, server that dies,
context timeout); the script rules (about 50 allowed and refused scripts for reads, 20 for changes);
settings (needs AWS, needs the script tool, admin only, off hides tools); assistant read scripts
(only reads reach the server, computed operations refused, developers refused, undeclared sandbox call
discarded); docs tools forwarding; the whole proposal path (assistant files, nothing runs, viewers and developers
cannot approve, wrong hash, own proposal, other administrator approves, exactly the filed script runs, no double approval,
reject, withdraw, expiry, connection change, extra sandbox call marked failed, one apply at a time).
Rules were removed one at a time and a test failed each time (proposer rule, hash, extra-call checks
for both paths, read guard, busy lock, expiry, target check, change rules, script role, script tool
requirement). In a browser with the real `claude` (developer account) against `scripts/dev/fake-aws-mcp`: an administrator turned the server on, the developer asked the assistant to scale an ECS service, the assistant filed a proposal with a read-then-update script (one earlier attempt was refused by the script rules and the model corrected itself), a second administrator reviewed the exact script and approved it, and the proposal showed the calls the sandbox reported. The browser run found three bugs, all fixed: the Proposals page returned a function instead of an element, a stray `0` rendered under the page, and the model wrote `call_boto3` with the API's arguments as keywords instead of `params` (now refused with an explanation). While the MCP server is on, `call_aws` (CLI) is turned off so there is one path to AWS.

## Not checked

- **Applying a change on the real AWS MCP Server.** Reads were probed on the real endpoint (tool list, `GetCallerIdentity`); no change was applied there.
- The sandbox's behaviour with scripts the stand-in does not understand (loops, pagination, many calls).
- Start-up time of the proxy per call (about 1 to 3 seconds in the probe) under load.
- The script rules are a reader, not a Python parser. They are layered with the sandbox's own `api_calls` check, and the strongest control is still the IAM role: run Forgeline's AWS profile with the least permissions that work.

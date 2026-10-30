# 015 AWS access for the assistant: `call_aws` now, the AWS MCP Server later

Status: `call_aws` **built** (2026-10-30). The AWS MCP Server connector is a design only.
Builds on notes 008 (assistant), 009 (AWS sign-in) and 013/014.

## Question

The AWS MCP servers expose a `call_aws` tool (and the managed AWS MCP Server adds `run_script`,
documentation search and more). Can the assistant use that, so it can look into an account (an ECS
cluster, security groups) and answer or propose changes?

## What the AWS MCP Server is (from the
[migration guide](https://github.com/awslabs/mcp/blob/main/src/aws-api-mcp-server/MIGRATION.md))

- The local `aws-api-mcp-server` is being replaced by the **AWS MCP Server**, a managed remote
  endpoint (`https://aws-mcp.us-east-1.api.aws/mcp`). Requests are signed with SigV4 from your local
  AWS credentials, usually through the `mcp-proxy-for-aws` stdio proxy, which should be pinned to an
  exact version, not `@latest`.
- Tools: `aws___call_aws`, `aws___run_script` (Python in a sandbox that chains API calls),
  `aws___search_documentation`, `aws___read_documentation`, `aws___retrieve_skill`,
  `aws___list_regions`, `aws___get_regional_availability`, `aws___get_presigned_url`, `aws___get_tasks`.
- Old environment variables (`READ_OPERATIONS_ONLY`, `REQUIRE_MUTATION_CONSENT` and others) no longer
  apply. Control is by **IAM** (policies and the `aws:ViaAWSMCPService` and `aws:CalledViaAWSMCP`
  condition keys, [AWS blog](https://aws.amazon.com/blogs/security/understanding-iam-for-managed-aws-mcp-servers)),
  a `--read-only` proxy flag, and CloudTrail records every call. The IAM action names are changing, so
  check the current documentation before writing policies.

## Decision

| Choice | Why |
| --- | --- |
| **Built now: a `call_aws` lookup in the assistant**, run by the Forgeline server with the AWS CLI and the credentials the console already holds | Works today with every backend, needs no new dependency, and keeps credentials out of the model. The guard is in our code, so it is testable here |
| **Not built: Forgeline as an MCP client of the managed AWS MCP Server** | It needs a signing proxy (Python tooling) on the server, a dedicated read-only IAM role, and a stable view of the IAM action names, which AWS says are changing. It also gives write tools unless the role forbids them. Worth doing, but as an optional connector after the role design is confirmed against current AWS docs |
| **Later: let agent steps use the AWS MCP Server** | A pipeline step running Claude Code in a container could use the proxy with a short-lived read-only role. That is the route for `run_script`-style multi-call analysis. Needs the OIDC / short-lived credential work in note 012 first |

## `call_aws` rules (built)

The model asks with `{"tool":"call_aws","args":{"cli_command":"aws ecs list-clusters"}}`. The server
parses one command and runs it without a shell. The tool is for **administrators only**, because it
reads more than the console's overview does.

| Rule | Detail |
| --- | --- |
| Read operations only | The operation must be `describe-*`, `list-*` or `get-*`. `sts` allows only `get-caller-identity` |
| Never, even if it looks like a read | Operations whose name mentions secret values, passwords, credentials, tokens, authorization, sessions, federation, decrypt, objects, log events or query results, console output, function or layer code, downloads, exports, presigned URLs, `get-parameter*`, key material |
| Services refused | `configure`, `sso`, `sso-oidc`, `secretsmanager`, `kms`, `s3` |
| Options refused | `--profile`, `--endpoint-url`, `--cli-input-json/yaml`, `--generate-cli-skeleton`, `--ca-bundle`, `--no-verify-ssl`, `--debug`, `--no-sign-request`, `--with-decryption`, `--output`, timeouts. Values that read files or fetch addresses (`file://`, `http://`, `s3://` and similar) |
| One command | No newline, pipe, redirect, `;`, `&`, backtick or `$` |
| Whose credentials | The console's AWS connection (profile, region, SSO role). `--region` is accepted if it is a valid region name |
| Size and time | 600 characters in, 12 KB out, 30 seconds, at most 6 lookups a question |
| Audit | Every call is recorded with its command (`assistant.lookup`, `call_aws: aws ecs ...`) |

## What was checked

Go tests with the race detector: about 45 allowed and refused command lines, including every
rule above; the chat path runs a read with the console's profile and region and `--output json`,
never runs a refused command, refuses developers and hides the tool from their prompt, tells the
model when AWS is not connected, and audits the command. Each rule was removed in turn and a test
failed (read-only rule, never-list, blocked options, blocked services, metacharacters, bad values,
`sts`, role, region). In a browser against `scripts/dev/fake-aws` (which now answers `ecs` and
security-group calls) with the real `claude`: it listed the clusters, the services and their details in three lookups and judged canary fit per service ([39](../console/39-assistant-call-aws.png)). Before a fix to the reply parser the model sometimes left off the opening fence; bare or half-fenced JSON that is the whole reply now runs, anything with words around it does not.

A browser run also exposed a bug in the `cli` backend: `--tools ""` removes only the built-in tools,
so the program could still call MCP servers from the server user's own setup. It now runs with
`--strict-mcp-config` and an empty MCP configuration.

## Not checked

- The managed AWS MCP Server itself: not called from Forgeline, and its IAM action names and
  condition keys were not verified against current AWS documentation.
- `call_aws` against a real AWS account (the SSO session had expired).
- Cross-account use, and accounts with many resources (output is cut at 12 KB; the model is told to narrow with `--query`).

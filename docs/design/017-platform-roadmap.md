# 017: accounts, agents, workflows, environments, guard, marketplace

Status: slice 1 built (this note); the rest planned. Builds on 013 (AI use cases), 014 (missions
and autonomy tiers), 015 and 016 (AWS access and the AWS MCP Server).

## The one rule everything keeps

Anything that changes the outside world is a **proposal** that a different administrator approves
against the exact change (note 016). Agents, skills, workflows and plugins can read, reason and
propose. None of them gets a new way to write. This is what lets us hand people more autonomy later
(note 014 tiers) without re-arguing safety for each feature.

## Slices, in build order

| # | Slice | What a user gets | Depends on |
|---|-------|------------------|------------|
| 1 | **Multiple AWS accounts** (built) | Register accounts by profile, switch between them in the console, explorer and spend per account, the assistant can read any of them | none |
| 2 | **Skills and agents** | A skill is a named, versioned procedure (markdown steps plus the read tools it may use). An agent is instructions plus skills plus a tool allowlist and an autonomy tier. The assistant can hand a task to an agent, and a person can talk to an agent directly | 1 |
| 3 | **Workflows** | Agents and steps in order or in parallel, with approval gates, started by hand, on a schedule, or when a run fails. Output is a report or proposals | 2 |
| 4 | **Guard** | Scheduled read-only security reviews per account (public buckets, open security groups, unencrypted volumes, idle resources, stale users), findings with severity and history, exceptions with an expiry, a pipeline guard (unpinned actions, secrets in logs) | 1, 3 |
| 5 | **Kubernetes** | Register clusters (kubeconfig or EKS), explorer for namespaces, workloads, pods and events, change proposals as a server-side dry-run diff, same approval | 1 |
| 6 | **Environments** | Temporary infrastructure and dev boxes for developers and QA from templates: owner, time to live, cost cap, auto-destroy, request then approve (or pre-approved class for small ones) | 1, 5 |
| 7 | **Marketplace** | Catalogue of agents, skills, workflow and environment templates, and plugins. Install shows the permissions it asks for. Versions pinned by hash. Plugins are MCP servers run in a container, never in the server process | 2, 3 |
| 8 | **Per-account writes** | One MCP proxy per account so approved changes can apply outside the primary account; cross-account role assumption; AWS Organizations discovery | 1 |

## Slice 1: how several accounts work

- The connection set up before (Infrastructure → AWS account) stays the **primary** account. It is
  the only one changes can be applied to, because the AWS MCP Server proxy is started for one profile.
- **Added accounts** are a profile and a region from the server's AWS configuration, with a name and
  an environment label (production, staging, development, sandbox). Adding one proves it works with
  `sts get-caller-identity` and keeps the account id, so the same account cannot be added twice.
- Reading works for every account: the explorer, the stacks and instances, spend, and the
  assistant (`aws_accounts` lists them; `aws_summary` and `call_aws` take an `account`).
- Changes against an added account are not offered. The console says so and the proposal tools
  keep their target binding to the primary account. Slice 8 lifts this.
- Only administrators add or remove an account. Removing one changes nothing in AWS.

## Slice 2 sketch (so slice 1 does not paint us into a corner)

```yaml
kind: agent
name: cost-watcher
instructions: Look for idle or oversized resources and propose the smallest safe change.
skills: [aws-idle-resources, ecs-rightsizing]
tools: [aws_summary, aws_script, propose_aws_change]   # a subset of the assistant lookups, never more
tier: T1                                              # advisory, review, pre-approved class (014)
accounts: [prod, staging]
```

A skill carries the steps and example scripts; the server composes the agent's prompt from them and
checks every tool call against the allowlist, exactly as the assistant's lookups are checked now.
Agents appear as lookups for the assistant (`ask_agent`), so the assistant can delegate, and as
their own chat.

## More features worth building (ranked by value over effort)

1. Failure summary on the run page, and a one-click "fix this pipeline" proposal (013 A1).
2. Cost anomaly watch per account, weekly, with the top three savings as proposals.
3. Drift: compare a stack's template with the account, and an infrastructure-as-code adoption mission
   that turns an existing cluster into a stack template (014).
4. Canary and blue/green deploy proposals for ECS, with the rollback already written.
5. Approval policies: who may approve what, quorum for high risk, office hours, break-glass with a
   reason that is audited.
6. SSO and SCIM for people, and an organisation-wide audit export (SIEM).
7. Pipeline templates and a pipeline linter in the editor.
8. Ephemeral preview environments per pull request.
9. A Forgeline MCP server so Claude Code and other agents can read runs and file proposals.
10. Run insights: flaky steps, slowest steps, cache hit rates.

## Not decided

- Whether the marketplace is a hosted service or a git repository of signed manifests. Starting with
  a git repository: no new infrastructure, easy to review, hashes pinned in the install record.
- How an agent proves its tier is respected after an upgrade: probably the tier lives on the install
  record, not in the package, so a package update cannot raise it.

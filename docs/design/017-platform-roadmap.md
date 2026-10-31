# 017: accounts, agents, workflows, environments, guard, marketplace

Status: slices 1 (accounts), 2 (skills and agents, with versions) 3 (workflows), 4 (guard) and 5 (Kubernetes, read-only) 6 (environments) and 7 (marketplace, skills and agents) built; the rest planned. Builds on 013 (AI use cases), 014 (missions
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
| 2 | **Skills and agents** (built) | A skill is a named, versioned procedure (markdown steps plus the read tools it may use). An agent is instructions plus skills plus a tool allowlist and an autonomy tier. The assistant can hand a task to an agent, and a person can talk to an agent directly | 1 |
| 3 | **Workflows** (built) | Agents and steps in order or in parallel, with approval gates, started by hand, on a schedule, or when a run fails. Output is a report or proposals | 2 |
| 4 | **Guard** (built) | Scheduled read-only security reviews per account (public buckets, open security groups, unencrypted volumes, idle resources, stale users), findings with severity and history, exceptions with an expiry, a pipeline guard (unpinned actions, secrets in logs) | 1, 3 |
| 5 | **Kubernetes** (read-only built) | Register clusters (kubeconfig or EKS), explorer for namespaces, workloads, pods and events, change proposals as a server-side dry-run diff, same approval | 1 |
| 6 | **Environments** (built) | Temporary infrastructure and dev boxes for developers and QA from templates: owner, time to live, cost cap, auto-destroy, request then approve (or pre-approved class for small ones) | 1, 5 |
| 7 | **Marketplace** (skills and agents built) | Catalogue of agents, skills, workflow and environment templates, and plugins. Install shows the permissions it asks for. Versions pinned by hash. Plugins are MCP servers run in a container, never in the server process | 2, 3 |
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

## Slice 2: what was built

Skills and agents are append-only lists of immutable versions (fingerprint, author, note). An agent
version pins exact skill versions and holds a tool allowlist and a tier; running it narrows the
assistant (its prompt gets the instructions and skills, its lookups are cut to the allowlist) and
never widens it. Rollback copies forward. `ask_agent` lets the assistant delegate one level deep.
See `docs/guides/versioning.md` for the rules. The sketch below is the shape it started from.

## Slice 7: what was built

A package is a manifest of skills and agents with its own sha256 over the content. The catalogue
ships in the binary (`internal/server/market/*.json`); every manifest is checked at read time and a
damaged one is listed with the reason but cannot be installed. Install needs the hash the
administrator reviewed, so a changed package must be reviewed again. The install record holds the
tier (the most any agent from the package may reach); an agent's own tier is capped to it and a
proposal tool is dropped at T0. An update appends versions to the skills and agents it made, skips
unchanged ones, and cannot raise the tier. Uninstall disables the agents and keeps everything.

Not built: a git source of signed manifests (so third parties can publish), signatures beyond the
hash, workflow and environment-template packages, and plugins. A plugin is an MCP server in a
container that the server talks to over a socket, with no network unless its manifest asks and an
administrator allows it; it needs its own design note before code.

## Slice 3: what was built

A workflow version is stages of agent steps with pinned agent versions, an optional gate per stage,
an optional interval, and a run-as role of viewer or developer. A run is saved after every stage;
a gate leaves it `waiting` and a restart keeps it waiting, while a run that was mid-stage fails
with "server restarted". One run per workflow at a time. Cancel always wins over a step that
finishes later. Not built: triggers other than manual and interval (for example, when a pipeline
run fails), parallel stages, and conditions between stages.

## Slice 4: what was built

Guard is deterministic on purpose: rules run over the explorer's inventory plus S3 public-access
settings and the IAM credential report, so a scan is fast, free and repeatable, and a finding means the
same thing every time. The AI part reads and explains them (`guard_findings`) rather than deciding
what is a problem. Findings reconcile per source: each explorer service, plus `s3-access` and `iam`,
has its own "was it readable" flag, so a denied read leaves its findings open. Not built: GuardDuty,
Security Hub and CloudTrail signals, drift against a stack template, and turning a finding into a fix
proposal in one click (many fixes, such as revoking a rule, are outside what proposals may do).

The pipeline guard (built later) is a third scan source, `pipelines`, that needs no cloud account:
it reads saved pipelines (static rules over the parsed steps) and the first 2 MB of each step's log
in the 25 newest runs (credential formats). Its two services, `pipe-pipelines` and `pipe-logs`,
reconcile like any other for pipelines (fixing or deleting one resolves its findings), but a scan never
resolves a log finding: a leaked credential is not safe because its run scrolled away. An
administrator accepts it once it is revoked. A finding names the variable or the kind of
credential and never repeats a value. The log check cannot see a credential in a format it doesn't
know, and exact-value masking is the only protection for the rest (design note 005, T3). The failure
summary on the run page (`GET /v1/runs/{id}/failure`) is rule-based from the recorded exit code,
error and log tail.

## Slice 5: what was built

Read-only. A cluster is registered by an administrator: EKS through a connected AWS account
(`aws eks describe-cluster` once, then `aws eks get-token` per read, cached until a minute before it
expires, fetched by one caller at a time), or an API server and a service-account token kept in the
vault under an internal name steps can never see. The Kubernetes client has no method that is not a GET
and only requests paths written in the code. The API server address must be https (http only on this
machine) and never link-local or a metadata address. Guard rules read workloads and pods; each source
section (nodes, deployments, StatefulSets, DaemonSets, pods, services) has its own "was it readable"
flag. Tested against an in-process API server and `scripts/dev/fake-k8s`, **not against a real
cluster**: creating a throwaway privileged cluster was not permitted in the build environment.
Not built: kubeconfig import and exec plugins other than EKS, change proposals as a server-side
dry-run diff with approval, logs and exec, RBAC and network-policy checks, and multi-page lists beyond
500 items per kind.

## Slice 6: what was built

An environment is a CloudFormation stack made from a catalog template, with an owner, purpose, time
limit and cost estimate, recorded beside the ordinary cloud change that creates it. Requesting one
proposes that change; approving it is unchanged four-eyes unless the **self-service policy** (off by
default, admin-set: templates, longest request, cost limit, environments per person) covers the request,
in which case the engine approves it in the name of `policy:self-service`. That is the one place a
stack is created without a second person, and the flag is decided by the server when the request is
made, never by the client. Deleting is automatic when time is up (`policy:expiry`), after a failed
creation (`policy:cleanup`) and when the owner or an administrator asks; each is a normal stack-delete
change and audited. An environment can only delete its own recorded stack. Extension is capped by the
longest life and, for non-administrators, a count. The engine reads state every 5 seconds and every
step is recorded, so a restart carries on. Templates are validated against CloudFormation and tested
against a stand-in; the real-account run is the next check. Not built: more templates, per-person
budgets across environments, a shared "team" owner, scheduled stop/start to save money, and requesting
an environment from the assistant.

Later: the assistant, review-tier agents and workflow steps can request an environment
(`environment_templates`, `request_environment`). It is the person's own request with `ViaAI` set, which
forces approval by a different administrator regardless of the self-service policy; scheduled workflows
(no person) and viewers can't ask; the per-person limit applies. Three templates were added (Lambda
sandbox, event pipeline, workflow sandbox), linted with cfn-lint and exercised through the fake CLI;
**not deployed to a real account** (no AWS access in the build that added them).

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

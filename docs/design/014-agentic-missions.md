# 014 Agentic missions: discover, propose, approve, apply, verify

Status: design note, 2026-10-30. Nothing here is built. It extends note 013 (AI use cases) from
"the assistant answers and drafts" to "an agent carries a goal through to a verified change, and a
person approves each step that changes something".

## The idea in one paragraph

A person gives Forgeline a goal in plain words: "look at our ECS cluster `payments` and give me
infrastructure as code and a CI/CD pipeline for it, with a canary deployment". The agent works as
a **mission**. It reads the real account, writes the proposal, shows the exact change, waits for a
person to approve, applies it through Forgeline's normal change path, checks that it worked, and
rolls back or reports if it did not. The model never holds cloud credentials and never approves
its own work.

## Evidence (read this first)

Sources are vendor docs and blogs, so they show patterns, not benchmarks.

- ECS now has built-in **canary and linear** deployments next to blue/green, for services behind an Application Load Balancer or Service Connect, configurable from CloudFormation, CDK, Terraform, CLI and console. Lifecycle hooks can be a Lambda or a pause point, and CloudWatch alarms with the deployment circuit breaker can trigger rollback ([announcement](https://aws.amazon.com/about-aws/whats-new/2025/10/amazon-ecs-built-in-linear-canary-deployments), [linear docs](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/deployment-type-linear.html)). The sources disagree on the overall deployment time limit, so read the current docs before choosing values. Both revisions run at once during a shift, which can double cost for a while.
- CloudFormation's **IaC generator** scans resources not yet managed by CloudFormation and builds a template from the ones you pick. Scans are per region, expire after 30 days, can be partial, and miss resources the caller cannot read; one template holds at most 500 resources ([docs](https://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/generate-IaC.html)). Terraform has declarative `import` blocks, but I found no official agent support, only third-party tools, one of which documents wrong-ID failures.
- Published agent designs for infrastructure converge on the same shape: read, analyse, propose, human approves, agent applies, verify. They add tiered autonomy (pre-approved low-risk fixes automatic, everything else waits), a rollback point per change, no self-approval, and advisory-only for security-critical and production core infrastructure ([CloudThat design](https://www.cloudthat.com/resources/blog/building-a-gen-ai-security-and-compliance-agent-on-aws), [forum thread](https://tianpan.co/forum/t/ai-agents-that-auto-remediate-your-infrastructure-code-dream-or-nightmare/1252), [Cloud Janitor](https://pypi.org/project/cloud-janitor/0.1.0/)).
- Not found: an independent evaluation of how often such agents get infrastructure changes right. Measure our own approval and rollback rates from day one.

## 1. What a mission is

A mission is a saved record, visible in the console, with these fields:

| Field | Meaning |
| --- | --- |
| goal | The person's words |
| scope | The account, region and resources the agent may read, chosen up front |
| stage | `discover`, `plan`, `awaiting approval`, `applying`, `verifying`, `done`, `rolled back`, `failed` |
| findings | Facts the agent read, each with where it came from (API call and time) |
| proposals | One or more changes, each with the exact diff, risk level and reason |
| evidence | Check results after applying |
| trail | Every tool call, every approval, who and when (goes to the audit log) |

A mission can be paused, cancelled or handed to another person. Its budget (tool calls, tokens,
minutes) is set at the start and stops it when used up.

## 2. Worked example: existing ECS cluster to IaC, CI/CD and canary

1. **Discover (read only).** The agent lists the cluster's services, task definitions, load balancers, target groups, scaling policies, log groups, IAM roles, secrets references and alarms. Every fact is stored as a finding. It cannot read secret values.
2. **Generate IaC.** It starts a CloudFormation IaC generator scan for those resource types, builds a template and **checks it with a dry run** (a change set that must show no changes against the live resources, otherwise the template is wrong). Result: a proposal "adopt these 14 resources into stack `payments`" with the template and the empty change set as evidence. Applying it changes nothing in the account except bringing the resources under management, and that import is the first thing a person approves.
3. **Generate the pipeline.** It writes a Forgeline pipeline: build image, test, scan, push to the registry, deploy by updating the stack. Network and secrets permissions are the narrowest that work (`network: none` for steps that do not need it). It is saved as an unapproved draft with a diff.
4. **Propose the canary.** A second proposal changes the service's deployment strategy to canary (for example 10% for a bake time, then the rest), adds the alarms that must stay quiet during the bake, and sets the rollback to the circuit breaker. The proposal shows the cost note (two revisions run at once) and says what will happen if an alarm fires.
5. **Approve.** A different administrator reviews each proposal in the console: exact diff, the findings it rests on, the model's reasons, the risk label. Approve, reject, or comment and send it back.
6. **Apply.** Forgeline applies it itself through the existing cloud-change path (CloudFormation), under its own rules. The model asked; it did not act.
7. **Verify.** The mission watches the checks it said it would: stack status, service steady state, target health, alarms quiet through the bake time. If a check fails, it proposes a rollback and, for changes marked auto-rollback, performs it.
8. **Report.** A summary page: what changed, evidence, what to do next, and the audit trail.

The same mission shape covers blue/green (same steps with a different strategy and a pause hook
that waits for a person to continue the shift) and a Terraform variant (generate `import` blocks
and run `plan`; the plan output is the evidence).

## 3. Other missions that fit the same shape

| Mission | Reads | Proposes | Verifies |
| --- | --- | --- | --- |
| **Security review** | Security groups, IAM policies, public buckets, unencrypted volumes, open ports, secrets in task definitions, container image findings, pipeline permissions | A ranked list of findings and, for each, a patch to the IaC | The finding clears; nothing else changed |
| **Least-privilege tightening** | IAM roles and what they were seen to use | A smaller policy | No access-denied errors in a watch window |
| **Drift check** | Live resources against the stack | Update the template or revert the drift, the person chooses | Stack and live state match |
| **Cost review** | Idle instances, oversized tasks, old snapshots | Stop, resize, delete (deletes are advisory only) | Cost down, service healthy |
| **Pipeline hardening** | Pipeline files and their permissions | Pin images by digest, drop network or secrets, add scan steps | Pipeline still passes |
| **Incident helper** | Failed run, deploy events, alarms, recent changes | Likely cause, a rollback or a fix proposal | Alarm clears |
| **Migration** | A GitHub Actions or Jenkins file | Forgeline pipeline plus a list of differences | Same result on a test run |
| **Environment copy** | A working stack | A staging copy with smaller sizes | Staging passes the smoke test |

## 4. Autonomy and approval tiers

Every proposal carries one tier. The default for a new installation is the strictest.

| Tier | What it covers | Who decides |
| --- | --- | --- |
| **T0 Advisory** | Anything the organisation marks protected (production core, IAM, networking, security groups, data stores), deletes, anything the agent cannot show a clean dry run for | The agent only writes the recommendation. A person applies it by hand or promotes it to T2 |
| **T1 Review** | Normal changes | Second administrator approves each proposal, one by one |
| **T2 Pre-approved class** | A named low-risk class the administrators enabled in advance (for example "add a missing tag", "enable encryption on a new volume", "re-run a flaky step") | Applied automatically with a rollback point, and listed for review afterwards |
| **Never** | Approving, changing approval rules, changing roles, reading secret values, deleting data stores, disabling logging or audit | Not available to any agent |

Rules that hold at every tier: the asker and approver are different people; the agent can never
approve; approval is bound to the exact content (if the proposal changes, the approval is void);
approvals expire; a mission that exceeds its budget stops; and each applied change records its
rollback.

## 5. How this is built on what exists

| Need | Exists | To build |
| --- | --- | --- |
| Apply cloud changes with a second approver | Yes (cloud changes, note 009) | Attach a mission and an AI-authored label to a change; bind approval to the content hash; add expiry |
| Read AWS safely | AWS sign-in and overview | A fixed set of read tools, each limited by the scope chosen for the mission, results wrapped as untrusted (note 013 A2) |
| Pipelines saved as versions | Yes | "Draft by agent" status with diff review |
| Run an agent with tools in a container | Yes (agent steps, note 007) | Source checkout (note 012 item 1) for pipeline and IaC work in a repo |
| Audit | Yes | Mission id on every entry, export |
| Mission record, budget, stages, UI | No | New: mission store, state machine, stage page with findings, proposals, evidence |
| Dry-run evidence | No | Change sets (CloudFormation) and `terraform plan` captured and shown in the proposal |
| Verification checks and rollback | No | A small check library (stack status, service steady state, alarm state, HTTP probe) and rollback proposals |
| Pre-approved classes | No | Administrator-defined policies, off by default |
| Protected resource list | No | Organisation setting with defaults |

## 6. Safety analysis

| Risk | Control |
| --- | --- |
| Instructions hidden in a resource name, tag, log or description steer the agent | Everything read from the account is wrapped as untrusted data. A write only ever becomes a proposal. The approver sees the diff, not the model's summary alone |
| Agent exceeds scope | Read tools check the mission scope on the server, not in the prompt |
| Agent holds powerful credentials | It holds none. Forgeline applies changes with its own role, which should itself be least-privilege and per environment |
| Approval fatigue, rubber-stamping | Risk label, a short plain-language "what will happen and what could go wrong", and a rule that big proposals are split into small ones |
| Silent partial failure | Verification is part of the mission; it is not "done" until checks pass |
| Runaway cost | Per-mission token and tool-call budget, org monthly cap |
| IaC generator output is wrong or incomplete | Clean dry run required before any adopt step; unmanaged and unreadable resources are listed as gaps, not hidden |
| Production blast radius | Protected list defaults to T0; first use on a staging account |

## 7. Suggested build order

1. **M-A: read-only missions.** Security review and drift check, scope picker, findings, report. No writes, so the safest place to learn how useful and accurate it is. (Needs note 013 A2.)
2. **M-B: IaC adoption proposals.** IaC generator scan, dry-run evidence, proposal through the existing second-approver path, verify.
3. **M-C: pipeline drafts.** The agent writes a pipeline for the adopted stack; reviewed as a draft.
4. **M-D: deployment strategy changes.** Canary and blue/green proposals with alarms and rollback; verification during the bake time.
5. **M-E: pre-approved classes and auto-rollback.** Only after we have real approval and rollback data from M-A to M-D.
6. **M-F: outside access.** The Forgeline MCP server (note 013 A6) so Claude Code can start and follow missions.

## 8. Decisions needed

1. Start with read-only security and drift missions (M-A), or go straight to the ECS example (M-B to M-D)? Recommendation: M-A, then the ECS example on a staging account.
2. IaC target: CloudFormation first (matches the existing change path) or Terraform first (what most teams already use)? Recommendation: CloudFormation first, Terraform second.
3. Is there an ECS cluster in either AWS account we can use for a real trial? The demo stack is one instance, not a cluster, so the ECS example would need a small test cluster created first (a small, costed step that needs your approval).

## 9. What was not verified

- No mission code exists; the ECS and IaC generator behaviour above comes from AWS documentation and was not run.
- ECS limits and timeouts are quoted from docs that disagree with each other in places.
- No evidence yet on agent accuracy for infrastructure changes. The first milestones exist to produce it.

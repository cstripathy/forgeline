# 004: Onboarding wizard and connectors

Status: **Proposed; partly overtaken.** Date: 2026-10-24. The web console (note 006) now has a
read-only AWS view (stacks, instances), a GitHub connection with a connection test, and an
approval flow for cloud changes, which covers parts of O1 and O4 in a different shape. ECS and
ECR discovery, the plan and apply engine and the generated pipelines are not built. Re-scope
O1 to O6 against the console before starting, and still ask the maintainer the three decisions
at the end.

## What we want

After signing in, a first-time user sees a wizard that:

1. shows connectors (GitHub, AWS) and tests each one;
2. discovers what exists (a GitHub repository, an ECS cluster and its services) and lets the user
   tick what to bring in;
3. produces a **plan**, shows exactly what it will create or change, and applies it once the user
   approves;
4. tests the result;
5. shows the CI/CD workflow from a GitHub push to an AWS deployment, in plain language and as a
   diagram.

It should be easy for a beginner, and later an AI agent should be able to do the picking.

## Principles that constrain the design

- **No secrets without a model for them.** M2 deliberately has no secrets management, and the
  trust model (principle 2) says a step must not combine untrusted input, secrets and external
  actions unless each is declared and granted. So the first version stores no credentials.
- **Humans approve changes.** Discovery is read-only and automatic. Anything that creates or
  changes something in a user's account happens only after an explicit approval of a specific
  plan (principle 5), and is audited.
- **Least privilege, shown before it is granted.** The wizard prints the exact IAM policy it
  wants to create.
- **Local first.** The whole wizard works on a laptop with the user's own credentials.

## Concepts

| Term | Meaning |
| --- | --- |
| Connector | A kind of external system: `github`, `aws`. Code in the Forgeline repo |
| Connection | A configured, tested instance of a connector (an AWS account and region, a GitHub user or org) |
| Resource | Something discovered through a connection: `github.repository`, `ecs.cluster`, `ecs.service`, `ecr.repository` |
| Blueprint | A known recipe over resources: "deploy a GitHub repository to an ECS service" |
| Plan | The ordered list of changes a blueprint would make for chosen resources. Pure data, computed without touching anything |
| Change | One step of a plan: target system, create or update or no-op, a plain-language line, the exact payload, a risk label, and the permission it needs |
| Apply | Executing an approved plan. Each change is idempotent, recorded, and safe to retry |
| Check | A test run after apply, or on demand, that proves a connection or a result works |

## Credentials, in two stages

**Stage A, the first release: no stored credentials.** The Forgeline process uses the operator's
own ambient credentials.

- AWS: the standard credential chain (environment, profile, SSO, instance role). The UI shows
  "connected as `arn:...` in account `...`, region `...`" from `sts:GetCallerIdentity`.
- GitHub: a token the operator supplies in the environment (a fine-grained token limited to chosen
  repositories), or one read from the `gh` CLI if present. Device-flow login is a later option.

Nothing secret is written to the database, returned by the API, or logged. Because it is the
operator's identity, this suits a laptop and a private single-user server. It is **not** suitable
for a shared server, because everyone who can sign in would act with the operator's credentials.

**Stage B, after M3: stored connections for shared servers.** AWS through an assumed role in the
customer account with an external ID (no long-lived keys). GitHub through a GitHub App whose
short-lived installation tokens are minted per run. Secrets are encrypted at rest, never returned,
and handed to a step only if its declared permissions say so. This stage waits for the threat
model and secrets design in M3.

## How AWS changes are applied: CloudFormation

For everything the wizard **creates** in AWS (an ECR repository, the deploy IAM role and policy,
log groups), the plan is a CloudFormation template and the apply is a stack.

- The plan screen is a real CloudFormation **change set**: AWS itself says what will be added,
  modified or replaced, before anything happens.
- Rollback on failure is built in, and so is removal: deleting the stack deletes exactly what the
  wizard made and nothing else. The demo stack in `deploy/aws/` already works this way.
- Everything is tagged `forgeline:managed=true` and `forgeline:plan=<id>`.
- The wizard never edits a resource it did not create, with one deliberate exception: a deploy
  pipeline registers new task definition revisions and updates the service to use them.

## ECS: what "all capabilities" means

"Everything ECS can do" is not a finite, safe thing to automate, so version 1 states its scope.

| Capability | Version 1 |
| --- | --- |
| Clusters, services, task definitions, tasks | Discovered (read-only) |
| Fargate and EC2 launch types, capacity providers | Discovered; deploy works for both |
| Rolling-update deployments (the ECS controller) | **Managed:** build, push, new task definition revision, update service, wait until stable |
| Deployment circuit breaker with rollback | Checked; wizard offers to enable it as a reviewed change |
| Load balancer target groups, health checks | Discovered, used in the post-deploy check |
| Log groups and recent task logs | Discovered, shown in the UI |
| Service auto scaling policies | Discovered (read-only), never modified |
| IAM task and execution roles | Detected; `iam:PassRole` in the deploy policy is limited to exactly those roles |
| Blue/green with CodeDeploy, Service Connect, service discovery, EBS volumes, ECS Exec, scheduled tasks, ECS Anywhere | **Detected and labelled "not managed yet".** The wizard says so and does not guess |

Unsupported setups are reported, never half-configured.

## The GitHub to AWS workflow

The wizard offers two paths and recommends one by what it finds:

**Path 1, Forgeline runs CI.** A GitHub webhook (M2 step 11) starts a Forgeline pipeline, which
assumes the deploy role: checkout, test, build image, push to ECR, register task definition,
update service, wait until stable, roll back to the previous revision if it does not stabilise.
Needs the server to be reachable from GitHub. A laptop is not, so locally this path is run by hand
(`forgeline run`) or through a tunnel the user sets up.

**Path 2, GitHub Actions runs CI.** The wizard generates `.github/workflows/deploy.yml` that signs
in to AWS with GitHub's OIDC token, so there are no stored AWS keys. The trust policy is limited to
one repository and one branch. Third-party actions are pinned to full commit SHAs, verified when
generated. The wizard opens a **pull request** with the file; it never pushes to the default branch.

The last wizard screen draws the chosen path (push, CI, image registry, service), says which
connection and permission each arrow uses, and links to the generated pipeline file.

## Testing a connection

| Connector | Checks |
| --- | --- |
| AWS | `sts:GetCallerIdentity`; region reachable; `iam:SimulatePrincipalPolicy` for each action the chosen blueprint needs, or the matching read call if simulation is not allowed; missing permissions listed as exact IAM actions |
| GitHub | Authenticated identity; the selected repositories are readable; for Path 1, a webhook ping is delivered; for Path 2, a pull request can be opened |
| After apply | The deploy role can be assumed; the ECR repository accepts a push of a small test image; the service is described and its current task definition is readable. A real deployment is **not** part of the default test, because it restarts tasks |

An optional "dry-run deploy" runs the generated pipeline up to, and not including, the service update.

## Screens and API

Screens: Welcome, Connect, Discover, Plan, Setup progress, Verify, Your CI/CD. The wizard state is
saved on the server so a refresh or a closed tab loses nothing.

API additions (all under `/v1`, same bearer token): `GET/POST /connections`,
`POST /connections/{id}/test`, `GET /discovery?connection=...`, `POST /plans` (pure: selections in,
plan out), `GET /plans/{id}`, `POST /plans/{id}/approve`, `POST /plans/{id}/apply`,
`GET /plans/{id}` for progress. Approvals and applies write audit records (who, when, which plan
hash, which changes).

## AI agents, later

A plan is structured data, so an agent can propose selections and a plan through the same API with
a token that may **create plans but not approve or apply them**. A human approves, and the audit log
records agent proposals separately from human approvals. The agent never receives credentials. It
sees discovery results, which can include sensitive names, so the connection owner decides whether
an agent may see discovery output at all.

## Implementation order

| Step | Content | How it is checked |
| --- | --- | --- |
| O1 | Connector framework, AWS read-only (identity, ECS and ECR discovery), Connect and Discover screens | Unit tests against a fake AWS API; a real read-only run against the maintainer's account |
| O2 | Plan engine and the `ecs-deploy` blueprint; CloudFormation template and pipeline generation | Golden-file tests; generated pipelines must pass `pipeline.Load` and `Plan`; CloudFormation `ValidateTemplate` |
| O3 | Apply engine: approve gate, change sets, audit log, retry, teardown | Real apply and delete against a scratch ECS cluster in the maintainer's account (small cost, disclosed first) |
| O4 | GitHub connector, repository listing, webhook (needs M2 step 11), pull request with workflow or pipeline file | Fake GitHub API tests; real run against a throwaway repository |
| O5 | Wizard polish, local mode end to end, diagrams | Scripted browser test |
| O6 | After M3: stored connections, assumed roles, GitHub App, multi-user | Per the M3 threat model |

New dependencies: the AWS SDK for Go v2 (Apache-2.0, modular: config, sts, ecs, ecr, iam,
cloudformation), and either the standard library or `google/go-github` (BSD-3) for GitHub. Each is
added at its latest version with the licence noted in the commit.

## Decisions needed

1. **GitHub sign-in for Stage A:** a token from the environment or `gh` (works today), a GitHub App
   (best long term, needs you to register one), or both in order.
2. **Applying AWS changes:** CloudFormation change sets as described above (reviewable, rollbackable),
   or direct API calls (faster to build, harder to undo).
3. **Which CI/CD path first:** Path 1 (Forgeline runs CI) or Path 2 (generated GitHub Actions workflow).

## What this does not do

It does not replace Terraform or CloudFormation for general infrastructure, does not take over
resources it did not create, and does not claim to manage every ECS feature. Drift detection is
not in the first release.

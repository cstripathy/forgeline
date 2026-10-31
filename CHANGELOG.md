# Changelog

All notable changes to Forgeline. The format follows [Keep a Changelog](https://keepachangelog.com/),
and versions follow [Semantic Versioning](docs/guides/versioning.md). Change the `Unreleased`
section in the pull request that makes the change; a release moves it under its version and date.

## [Unreleased]

### Fixed
- A cloud change canceled while its preview was still being prepared came back as waiting for approval when the preview finished; the cancellation now stays.

### Added
- **Environments from the assistant, and three more templates.** The assistant, review-tier agents
  and workflow steps can request a temporary environment in a person's name (`environment_templates`,
  `request_environment`). The request never starts on its own, even under the self-service policy; a
  different administrator approves it, and the audit log and the change's reason say the assistant asked.
  New templates: Lambda sandbox, event pipeline (SNS to SQS with a dead-letter queue) and workflow sandbox
  (Step Functions), each free while idle.
- **Changes in any AWS account.** An administrator can allow change proposals for an added account
  (off by default). Cloud changes and the assistant's AWS change proposals can then name the account;
  each is bound to the account, its AWS account number, profile and region, runs through that
  account's own signing proxy, and is canceled if the account is removed or changes are switched off.
  (note 021)
- **Forgeline as an MCP server.** Claude Code and other AI clients can connect to `/mcp` (or through
  `forgeline mcp` for stdio) with a personal API token made on the Account page. They get the
  assistant's read-only lookups (runs, why a run failed, pipelines, Guard findings, AWS and Kubernetes
  views, agents, the audit log for administrators) and two propose-only tools whose changes a
  different administrator must approve. A token acts as its person with at most the role you give it
  (a demoted or disabled person loses it at once), expires, is stored as a hash, and works nowhere
  else. Every call is audited, output is cut and credential formats are masked, and calls are
  rate-limited. Raw AWS access, agents and plugin tools are not exposed. (note 020)
- **Pipeline guard.** Guard now reads saved pipelines and recent run logs, with no cloud account
  needed: unpinned images, secrets with open network, agent steps holding extra secrets, credentials
  or secret-looking values written into a pipeline file, `curl | sh`, scripts that print secrets, and
  credential formats in run logs. Findings never repeat the value.
- **Why a run failed.** A failed run shows a plain summary: the step that failed first, the likely
  causes, what did not run because of it, and the end of its output.
- **Plugins.** A marketplace package can carry MCP servers shipped as container images. They are off
  until an administrator turns them on (Docker must answer), and install grants nothing by default:
  network and each secret are separate choices. Each call runs in a fresh container that is
  read-only, non-root, has no extra privileges, no network unless granted and limits on memory, time
  and output. Their tools become read-only lookups an agent can be given; arguments are checked
  against a schema first and the output is treated as data. An update that asks for a different
  image, network or secrets needs a new grant. `FORGELINE_MARKET_DIR` adds a local directory of
  package manifests. (note 018)
- **Environments.** Temporary infrastructure and developer boxes from catalog templates (a developer
  box and a QA sandbox): each has an owner, a purpose, a time limit and a cost estimate, shown before
  you ask. Approval is the same four-eyes cloud change as ever, unless an administrator turns on a
  self-service policy for chosen templates within a time and cost limit; then those start on their own
  in the name of the policy and are audited. Forgeline deletes an environment when its time is up,
  cleans up a failed creation, and lets an owner extend (within a lifetime limit) or delete early. A
  person can have only so many at once. (note 017)
- **Kubernetes (read-only).** Register clusters (EKS through a connected AWS account, nothing stored;
  or any cluster with a service-account token kept in the vault), explore nodes, deployments,
  StatefulSets, DaemonSets, pods, services, namespaces and warning events, and let Guard check them:
  privileged containers, host networking, running as root, unpinned images, missing memory limits,
  workloads with no ready replicas, failing pods, nodes that are not ready, services exposed through a
  load balancer. The client can only send GET requests. The assistant and agents read clusters with
  `k8s_clusters` and `k8s_resources`. (note 017)
- **Guard.** Scheduled or on-demand read-only security and hygiene scans of every connected AWS
  account: open security groups, unencrypted volumes and databases, public databases, S3 public-access
  settings, the IAM credential report (root keys and MFA, users without MFA, old access keys),
  deprecated Lambda runtimes, failed stacks, stopped instances, logs that never expire, and more.
  Findings keep a stable id and history, resolve themselves when fixed, and are never resolved by a
  check that could not run. An administrator can accept a finding for up to 90 days with a reason; it
  reopens afterwards. The assistant and agents read them with `guard_findings`. (note 017)
- **Workflows.** Stages of agents run in order; steps in a stage run together; a stage can wait
  for an administrator; a workflow can run on an interval. Each step runs an agent at a version
  pinned when the workflow was saved, with each stage's output handed to the next as data. An
  unattended run is capped at developer (never an administrator) and anything it proposes still needs
  a person. Versioned like skills and agents, with run history, cancel, and recovery after a restart.
  (note 017)
- **Skills and agents with versions.** A skill is a procedure; an agent is instructions, pinned
  skill versions, a tool allowlist and a tier (T0 advises, T1 may file proposals). Every save is a
  new immutable version with a fingerprint, history, diff and rollback. The assistant can hand a
  task to an agent (`ask_agent`); an agent never calls another agent and never gets more than the
  person asking could do. (note 017)
- **Several AWS accounts.** Add read-only accounts by profile and region; switch between them in
  the console; the explorer, spend and the assistant read any of them. Changes stay on the primary
  account. (note 017)
- **AWS resource explorer** and the `aws_resources` lookup: every service in an account with state,
  search, detail, and what Forgeline changed there.
- **AWS MCP Server.** Read lookups, change proposals and four-eyes approval that applies exactly the
  approved script (note 016); the Proposals page.
- **Assistant lookups** (runs, logs, pipelines, AWS, audit) and a read-only `call_aws`
  (note 015).

### Changed
- Builds are stamped with the version from the nearest git tag.

## [0.0.0] - development

Console, pipelines, agent steps, assistant, AWS sign-in and GitHub App: see `docs/PLAN.md`.

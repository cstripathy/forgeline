# Changelog

All notable changes to Forgeline. The format follows [Keep a Changelog](https://keepachangelog.com/),
and versions follow [Semantic Versioning](docs/guides/versioning.md). Change the `Unreleased`
section in the pull request that makes the change; a release moves it under its version and date.

## [Unreleased]

### Added
- **Marketplace.** A catalogue of ready-made skills and agents (failed-run triage, cost watch,
  security review, cluster health). Each package shows what its agents may read and which tier they
  could reach before anyone installs it, and carries a sha256 that is checked when the server starts
  and again at install, so what is installed is what was reviewed. An administrator chooses the most
  an installed agent may do (T0, or T1 to let it file proposals a person still approves); an update
  adds new versions, never raises that, and skips what did not change. Uninstalling disables the
  agents and keeps their history. Plugins (containerised MCP servers) and a git-hosted source are not
  built yet. (note 017)
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

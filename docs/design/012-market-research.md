# 012 Market research: pain points, enterprise gaps, UI and integrations

Status: research note, 2026-10-30. Nothing here is built yet. It turns outside evidence plus an
honest look at the console into a ranked list of work.

## How much to trust this

A web search found **no independent 2026 survey** of CI/CD pain points. Most sources are vendor
blogs. The table marks each claim as **cited** (a source says it, usually a vendor),
**widely known** (general industry knowledge, not re-verified here) or **ours** (read from our
own code and screenshots). Numbers from vendors are indicative only. Before quoting any figure
in public material, check its original source.

## 1. Pain points and what they mean for Forgeline

| # | Pain point | Evidence | What Forgeline should do |
| --- | --- | --- | --- |
| P1 | Waiting: slow builds and queueing for a runner. One vendor survey reports about 57 minutes a day lost; the same vendor argues job start time is a queueing problem | cited ([Incredibuild](https://www.incredibuild.com/blog/ci-cd-pipeline-optimization-best-practices), [DEV](https://dev.to/kalemi/the-2026-cicd-squeeze-faster-code-shifting-prices-and-where-reliability-fits-1h16)) | Queue-time and cache-hit metrics on the overview; worker pools with labels; per-step caching already exists, extend to a shared remote cache |
| P2 | Flaky tests: retry and hope. One paper cites about 8% of builds failing from flakes (older data) | cited ([Frontiers](https://www.frontiersin.org/journals/artificial-intelligence/articles/10.3389/frai.2026.1776546/full)) | Per-step history, flake rate, quarantine with owner, reason and expiry (see P2 notes below) |
| P3 | YAML sprawl and slow feedback: a typo is found after a push | cited ([TechTarget](https://www.techtarget.com/searchapparchitecture/tip/Pipeline-as-Code-Managing-CI-CD-complexity-and-sprawl)); GitLab users also complain that testing a change needs a commit and push ([talk](https://docs.google.com/presentation/d/1G-tATqbn2LuHRMcdVdpw3NOTFcBacnvloFwvCEWloVM/htmlpresent)) | We already validate live in the editor and have `forgeline run` locally. Add reusable templates, inheritance and a dry-run plan that shows what would run and why |
| P4 | Supply-chain attacks through third-party actions and leaked secrets (the tj-actions/changed-files incident) | cited ([Cycode](https://cycode.com/blog/github-actions-supply-chain-attack/), [Apiiro](https://apiiro.com/blog/preventing-the-next-supply-chain-attack), [Unit 42](https://unit42.paloaltonetworks.com/?p=138681)) | Our model already helps: steps get no network or secrets unless declared, secrets are masked, GitHub-triggered runs get no secrets. Add: pin images by digest, allowed-image policy, egress allow lists, provenance |
| P5 | Long-lived cloud keys stored as CI secrets | cited ([Aembit](https://aembit.io/blog/securing-ci-cd-pipelines-the-role-of-workload-identity-federation)) and widely known | Issue short-lived OIDC tokens per run so steps assume a cloud role with no stored key |
| P6 | Pricing and lock-in surprises. GitHub announced a self-hosted runner fee for March 2026, then postponed it after the backlash; sources disagree on dates, so check GitHub's own changelog | cited ([The Platform Engineering newsletter](https://theplatformengineering.substack.com/p/github-actions-2026-pricing-changes), [DevClass](https://www.devclass.com/development/2025/12/17/github-to-charge-for-self-hosted-runners-from-march-2026/1734518)) | Position as self-hosted and open source with no per-minute fee. Keep GitHub as an integration, not a dependency |
| P7 | Tool sprawl: separate tools for source, CI, scanning and compliance; context switching | cited, self-reported ([DEV](https://dev.to/johalputt/why-we-replaced-jenkins-2460-with-gitlab-ci-1610-a-devsecops-opinion-piece-1b3g)) | One console that shows pipelines, secrets, cloud changes, audit and approvals together (we have this) and links out to scanners instead of rebuilding them |
| P8 | Poor failure triage in the UI: failed logs auto-open and push the graph off screen; graphs overflow; counts hide trends; no way to link to a log line | cited ([Jenkins issue](https://issues.jenkins-ci.org/browse/JENKINS-56639), [GitLab issue](https://gitlab.com/gitlab-org/gitlab/-/issues/19468), [GitLab analytics](https://gitlab.com/gitlab-org/gitlab/-/issues/583667), [CloudBees](https://cloudbees.com/blog/simplify-DevOps-pipeline-management)) | See the UI plan in section 4 |
| P9 | AI agents in pipelines with too much access | ours (this is the product's stated direction) | Keep agent steps read-only by default; add per-run budgets, approval gates before write tools, and an agent action log |

## 2. Enterprise features: what buyers ask for and where we stand

Vendor pages (Semaphore, Harness, WarpBuild) consistently list the same set. The list below is
that set, checked against what Forgeline has today (ours).

| Feature | Today | Gap | Priority |
| --- | --- | --- | --- |
| Roles (RBAC) | Fixed roles: admin, developer, viewer | Custom roles, per-pipeline and per-environment permissions | High |
| SSO (OIDC, SAML) and SCIM provisioning | Local accounts only | OIDC login first, then SAML, then SCIM; group-to-role mapping | **Top** (blocks most enterprise evaluations) |
| Audit log | Recorded and viewable | Export (JSON and syslog or S3), retention setting, tamper evidence (hash chain), filters | High |
| Approvals | Cloud changes need a second administrator | Approval gates on any step or environment, required reviewers, change windows | High |
| Policy as code | None | Org policies (allowed images, required scans, banned network) evaluated before a run; OPA or a small built-in rule language | High |
| Secrets | Encrypted vault, write-only | Rotation reminders, external stores (AWS Secrets Manager, Vault), OIDC instead of keys | High |
| Supply-chain evidence | Not built | SBOM generation step, build provenance (SLSA-style attestation), image signing | Medium (Harness advertises SLSA L2 and SBOMs; unverified) |
| Compliance | Design note 005 threat model | SOC 2 style control mapping doc, data retention controls | Medium |
| Multi-tenancy | One organisation | Projects or teams with their own pipelines, secrets and quotas | Medium |
| High availability | In-process locks, one server | Run state, locks and the scheduler safe across several servers (Postgres advisory locks) | **Top** (listed as a known gap in the plan) |
| Scale | One host's Docker | Remote workers with labels, autoscaling workers, ephemeral just-in-time runners (design note 011 slice 3) | High |
| Observability | Overview page | Prometheus `/metrics`, OpenTelemetry traces per run, webhooks out | High |
| Backup and upgrade | Not documented | Documented backup, restore, and tested migrations | Medium |

## 3. Integrations: what to add, in order

Order by how many users it unblocks, not by how easy it is.

1. **Git hosts:** GitHub (done: App, status, Actions), then **GitLab** and **Bitbucket** (webhooks, status, clone) and a generic webhook trigger. Repo checkout for console runs is still missing and is the biggest functional gap (agents and builds see an empty workspace).
2. **Identity:** OIDC then SAML (Okta, Entra ID, Google), SCIM.
3. **Notifications:** Slack, Microsoft Teams, email, generic outgoing webhook; PagerDuty for failures on protected branches.
4. **Clouds:** AWS (done), then Azure and GCP via OIDC federation, plus Kubernetes deploy steps.
5. **Artifacts and registries:** OCI registries (ECR, GHCR, Docker Hub), S3 or generic artifact store, package registries.
6. **Security scanners as steps:** Trivy, Grype, Semgrep, gitleaks, with results shown in the run page and gated by policy.
7. **Trackers:** Jira and Linear links from runs; create an issue on a quarantined flaky test.
8. **Import:** a converter from GitHub Actions workflow files and Jenkinsfiles to Forgeline YAML. Migration cost is the main reason teams stay on a tool they dislike.
9. **Extensibility:** a public API reference (done), API tokens with scopes, outgoing webhooks, and a step plugin interface.

## 4. UI: what is wrong and the plan

Reviewed from the current screenshots (`docs/console/`), so this is ours and visual only.

What works: clean layout, a status colour system, a live graph, dark mode, sensible empty
states. What to improve:

**Navigation and structure**
- Add a command palette (`Ctrl+K`) to jump to pipelines, runs, secrets and settings.
- Group the sidebar (Build: Pipelines, Runs; Operate: Infrastructure, Integrations; Govern: Secrets, People, Audit). There is no page that lists all runs across pipelines.
- Breadcrumbs exist; add a persistent environment and organisation switcher once multi-tenancy lands.

**Run page (the page people live in)**
- Collapse a failed step's log by default but pin a one-line failure summary above the graph; do not push the graph off screen (P8).
- Log viewer: search, line numbers, link to a line, download, ANSI colour, timestamps toggle, collapse by group, virtualised rendering for large logs.
- Show step timeline (Gantt) next to the graph so slow steps and queue time are visible.
- Re-run failed steps only; compare with the last successful run.
- Failure annotations: parse common test output and surface the failing test names.

**Overview and trends**
- Replace the "success rate over last 5 runs" tile with a ratio trend over a chosen period, plus p50 and p95 duration and cache-hit rate.
- Show the flaky-step leaderboard and the slowest steps.

**Editor**
- Today it is a plain text area beside a graph. Move to a real editor (syntax highlighting, inline errors with line numbers, autocomplete from the schema) and keep the step builder as a form view.
- Side-by-side diff in version history.

**Quality and accessibility (the "less bugs" part)**
- Run an automated accessibility check (keyboard order, contrast, labels) and fix findings; the console is hand-written JavaScript with no component tests.
- Add a browser test suite (Playwright) in CI that walks the main flows, replacing today's manual walkthroughs. Include mobile width.
- Add loading, error and offline states to every list; today several fetch failures surface as a blank section.
- Keep one design token set and document it (`docs/console/`), so new pages stay consistent.

## 5. Ranked backlog

Scored on user impact against effort. Each item should become its own small PR with a design
note if it touches trust.

| Rank | Item | Why now | Size |
| --- | --- | --- | --- |
| 1 | Repo checkout for console runs (and GitHub-triggered runs) | Without source, runs cannot build real projects or give agents context | M |
| 2 | Browser test suite in CI (Playwright) | Cuts bugs for every later UI change | M |
| 3 | Run page rework: failure summary, log viewer, timeline, re-run failed | Highest daily-use screen (P8) | M |
| 4 | OIDC login, group-to-role mapping | Blocks enterprise evaluation | M |
| 5 | Multi-server safety (locks and scheduler in Postgres) | Known correctness gap; needed for HA | M |
| 6 | Prometheus metrics and outgoing webhooks | Cheap, unlocks monitoring and notifications | S |
| 7 | Notifications: Slack, Teams, email | Most requested integration | S |
| 8 | Audit export and retention, tamper evidence | Compliance | S |
| 9 | Approval gates and policy rules | Enterprise governance | L |
| 10 | Short-lived OIDC credentials for cloud steps | Removes stored keys (P5) | M |
| 11 | Remote workers with labels; JIT runners (note 011 slice 3) | Scale (P1) | L |
| 12 | Flake tracking and quarantine with owner and expiry | P2 | M |
| 13 | Command palette, grouped navigation, overview trends | UI polish | S |
| 14 | GitLab and Bitbucket | Breadth | L each |
| 15 | Importers from GitHub Actions and Jenkinsfile | Lowers migration cost | L |
| 16 | SBOM, provenance and signing | Supply-chain evidence | L |

Quarantine rules worth copying (cited from [Buildkite](https://buildkite.com/docs/test-engine/quarantine), [Trunk](https://docs.trunk.io/flaky-tests/quarantining) and [Airtable](https://medium.com/airtable-eng/how-airtable-manages-flaky-tests-in-a-large-scale-monorepo-5fe09922e90c)):
quarantined tests keep running and reporting; each entry has an owner, a reason and an expiry;
automatic quarantine applies only on the default and merge-queue branches; recovered tests are
released automatically, because lists otherwise grow forever.

## 6. What this research could not tell us

- No independent survey data. Choose the top items by talking to real users, not by this table alone.
- Nothing found on complaints about CircleCI, Buildkite, Dagger or Argo Workflows. Search each separately before positioning against them.
- Remote build cache and merge queue needs had thin coverage.
- SLSA and SBOM claims come from one vendor and are unverified.

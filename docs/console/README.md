# The web console, in pictures

These screenshots were taken from a running server in a real browser while testing the console
(see [design note 006](../design/006-console.md)). Everything was done through the UI. Where a
step touched something outside Forgeline, the notes say what stood in for it.

| Picture | What it shows |
| --- | --- |
| [01-setup](01-setup.png) | First run: the first administrator is created, and the server token proves you control the server |
| [07-overview](07-overview.png) | Pipelines with current status, a sparkline of the last runs, success rate and average time; failing pipelines sort first; platform health strip |
| [04-editor-graph](04-editor-graph.png) | The pipeline editor: line numbers, live validation by the server, and a live graph |
| [30-step-builder](30-step-builder.png) | Adding a step from a form (needs, network, secrets, time limit) instead of writing YAML |
| [31-version-history](31-version-history.png) | Every save is a version; any can be viewed or restored |
| [05-run-live](05-run-live.png) | A run on real Docker: `build` done, `unit-tests` and `lint` running in parallel, logs streaming |
| [06-run-done-masked-secret](06-run-done-masked-secret.png) | The finished run. The step printed its secret; the stored output shows `***` |
| [08-run-failed](08-run-failed.png) | A failure: the failing step is selected, its dependents are skipped, exit code and stderr are shown |
| [10-secrets-list](10-secrets-list.png) | Secrets: name, where stored, which pipelines use it. Values are never shown |
| [32-vault-secret-run](32-vault-secret-run.png) | A secret created in the UI, used by a step built in the UI, with no network, masked in the output |
| [11-people](11-people.png) | People and roles |
| [12-github-form](12-github-form.png), [14](14-github-active-tested.png), [15](15-github-deliveries.png) | Connecting GitHub: repositories and branches, token (write-only), webhook secret (shown once), connection test, and the deliveries list (accepted, ignored, rejected). A stand-in GitHub API served the downloads and received the commit statuses |
| [23-deploy-preview](23-deploy-preview.png) | A cloud change as the approver sees it: reason, validated parameters, and exactly what would be created |
| [25-change-succeeded](25-change-succeeded.png) | The same change after a different administrator approved it and it ran. An AWS stand-in served the calls |
| [26-audit-log](26-audit-log.png) | The audit trail: setup, accounts, proposal, approval, execution |
| [28-overview-dark](28-overview-dark.png), [29-mobile-overview](29-mobile-overview.png) | Dark theme and a phone-width layout |

Also verified in the browser but not pictured here, because the pictures show an AWS account:
the Infrastructure page reading a real account (stacks, instances) with the AWS CLI profile of
the person running the server, and a change proposed against it that its own proposer could not
approve and a second administrator rejected, so nothing in that account changed.

| Picture | What it shows |
| --- | --- |
| [34-actions-runs](34-actions-runs.png) | GitHub Actions page: the workflows of an allowed repository, and its runs with status, branch, event and who started them. A run from a fork carries a `fork` tag. A stand-in GitHub served the data |
| [35-actions-run-log](35-actions-run-log.png) | A failed run: jobs and steps, the end of the job's log (shown as text), and Re-run buttons |
| [36-actions-dispatch](36-actions-dispatch.png) | Starting a workflow: only branches allowed for the repository, optional inputs |
| [37-workflow-step-builder](37-workflow-step-builder.png) | Adding a GitHub Actions workflow step from the form |
| [38-workflow-step-run](38-workflow-step-run.png) | A run with a container step followed by a workflow step, its log showing GitHub's progress. A stand-in GitHub served the calls |

## Assistant looking at an ECS cluster (call_aws)

![The assistant listing an ECS cluster's services and judging canary fit](39-assistant-call-aws.png)

Three read-only `call_aws` lookups (clusters, services, service details) against the AWS stand-in
`scripts/dev/fake-aws`, answered by the real `claude`. Design note 015.

## AWS MCP Server and change proposals

![AWS MCP Server switch](40-mcp-settings.png)

![Proposals list](41-proposals-list.png)

![A proposal under review: why, the AWS calls, the exact script, approve or reject](42-proposal-review.png)

![The same proposal after a different administrator approved it](43-proposal-applied.png)

### AWS resource explorer

Infrastructure → AWS account lists everything in the connected account by service (ECS clusters and
services, EC2, Lambda, S3, RDS, DynamoDB, VPCs, load balancers, queues, log groups and more), with
state, search, a detail panel per resource, and a mark on anything an approved proposal changed. It
only reads, and a service the role may not list is named instead of shown as empty.

![The resource explorer on a real account, with the cluster created through an approved proposal](44-aws-explorer.png)

The assistant drafted the change, another administrator approved the exact script, Forgeline applied
it through `scripts/dev/fake-aws-mcp` (a stand-in; design note 016).

### Several AWS accounts

Infrastructure → AWS account has an account switcher. The connection made first is the primary
account; administrators can add more by profile and region (Add account proves the profile with
`sts get-caller-identity`, labels it production, staging, development or sandbox, and refuses an
account that is already added). Every account can be read: the explorer, stacks, spend, and the
assistant (`aws_accounts`, then `aws_summary` or `call_aws` with an `account`). Changes are proposed
for the primary account only; the page says so on the others. Removing an account changes nothing in AWS.

![An added account (a second real AWS account) in the explorer](45-aws-accounts.png)

### Agents and skills

The Agents page lists agents and skills. A **skill** is a procedure written in plain steps. An
**agent** is instructions, pinned skill versions, a short list of tools and a tier (T0 advises, T1
may also file change proposals). Every save is a new version with a fingerprint, its author and a
note; the page shows the diff from the previous version, flags when a pinned skill has a newer
version, and can roll back by copying an old version forward. Talk to an agent on its page, pick
any past version to run, or let the assistant hand it work (`ask_agent`). An agent uses only its own
tools, every one of them runs as the person asking, and an agent cannot call another agent.

![An agent with a pinned skill, its tools and its version history](46-agent.png)


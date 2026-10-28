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

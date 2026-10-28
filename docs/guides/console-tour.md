---
title: Console tour
parent: Guides
nav_order: 1
---

# Console tour

Everything below is done from the web console. Pictures were taken from a running server in a real browser.

## Overview
Every pipeline with its current status, a sparkline of its last runs, success rate and average time. Failing pipelines sort first. A strip at the top shows platform health.

![Overview]({{ '/console/07-overview.png' | relative_url }})

## Pipelines
A YAML editor with line numbers and validation by the server as you type, plus a live graph. **Add step** builds a step from a form (needs, network, secrets, time limit, or a Claude Code agent). Every save is a version you can view or restore.

![Editor]({{ '/console/04-editor-graph.png' | relative_url }})
![Step builder]({{ '/console/30-step-builder.png' | relative_url }})
![Versions]({{ '/console/31-version-history.png' | relative_url }})

## Runs
The steps as a graph coloured by status, with live logs. A failure selects the failing step, skips its dependents and shows the exit code and stderr.

![Failed run]({{ '/console/08-run-failed.png' | relative_url }})

## Secrets
Names, where each is stored and which pipelines use it. Values are write-only: nobody can read one back, and a step's output shows `***` where it printed one.

![Secrets]({{ '/console/10-secrets-list.png' | relative_url }})

## Actions
Start, watch and re-run GitHub Actions workflows of your allowed repositories. See [GitHub Actions]({{ '/guides/github-actions/' | relative_url }}).

![Actions]({{ '/console/34-actions-runs.png' | relative_url }})

## Infrastructure
Three tabs: **Platform** (the Forgeline server, Docker host and running steps), **AWS account** (identity, stacks, instances, spend) and **Cloud changes** (proposals and approvals). See [Connect AWS]({{ '/guides/connect-aws/' | relative_url }}).

![Cloud change]({{ '/console/23-deploy-preview.png' | relative_url }})

## Integrations
GitHub (App or token), the assistant, and AWS. See [Connect GitHub]({{ '/guides/connect-github/' | relative_url }}) and [Assistant and agents]({{ '/guides/assistant-and-agents/' | relative_url }}).

## People and audit log
Users and roles, and an append-only record of who did what, including refused actions and failed logins.

![Audit log]({{ '/console/26-audit-log.png' | relative_url }})

## Dark theme and phones
Both are supported.

![Dark]({{ '/console/28-overview-dark.png' | relative_url }})

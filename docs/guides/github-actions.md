---
title: GitHub Actions
parent: Guides
nav_order: 4
---

# GitHub Actions

Use **Actions** in the console to start, watch and re-run your GitHub workflows without leaving
Forgeline. Forgeline only talks to GitHub's API; GitHub runs the jobs.

![Actions page]({{ '/console/34-actions-runs.png' | relative_url }})

## Before you start

1. [Connect GitHub]({{ '/guides/connect-github/' | relative_url }}). The page lists only the repositories on the allow list.
2. Give the GitHub App the **Actions: read and write** permission. Open the App's page on GitHub and approve the new permission (GitHub asks the App's owner). With a token instead of an App, the token needs the `repo` scope (classic) or the Actions permission (fine-grained).
3. For **Run**, the workflow file needs `on: workflow_dispatch`.

If the permission is missing you see: *the GitHub App does not have the Actions permission yet*.

## What you can do

- **See** workflows and the latest 30 runs of a repository. Click a workflow to filter. Runs that are still going refresh every 10 seconds.
- **Run** a workflow on an allowed branch, with optional inputs.
- **Open a run** to see jobs and steps, read the end of a job's log, **cancel**, **re-run all jobs**, or **re-run failed jobs**.

![A failed run]({{ '/console/35-actions-run-log.png' | relative_url }})
![Starting a workflow]({{ '/console/36-actions-dispatch.png' | relative_url }})

## What keeps it safe

- Only repositories on the allow list, and only branches their rule allows. Starting a workflow runs that branch's workflow file, so a branch you would not trust to push to is refused.
- Developers and admins only. Every start, cancel, re-run and refusal is in the audit log, and writes are limited to 20 a minute per person.
- Logs and run names come from GitHub, so they are shown as plain text. The GitHub token is never sent to the address a log is downloaded from.
- A run whose code comes from a **fork** is tagged and carries a warning.

## Start a workflow from a pipeline

A step can start a workflow and wait for it, so a Forgeline pipeline can include your existing GitHub jobs:

```yaml
steps:
  - id: prepare
    image: alpine:3.24
    run: echo preparing
  - id: ci
    needs: [prepare]
    timeout: 30m            # how long to wait
    workflow:
      repo: acme/widgets
      workflow: ci.yml
      ref: main
      inputs: {environment: staging}
      # detach: true        # do not wait
```

In the editor use **Add step → Kind → GitHub Actions workflow**.

![Step builder]({{ '/console/37-workflow-step-builder.png' | relative_url }})
![A run with a workflow step]({{ '/console/38-workflow-step-run.png' | relative_url }})

- It runs on the server, with the server's GitHub connection. No GitHub credential ever enters a container, and the step takes no image, script, environment or permissions.
- It succeeds only if the workflow's conclusion is `success`. If you cancel the Forgeline run, or the step times out, the GitHub run is canceled as well.
- The repository and branch are checked when the step runs, against the allow list. A refusal is in the audit log.
- **Runs started by GitHub cannot use it**, because the pipeline in a push or pull request is not trusted to start more work.
- `forgeline run` on your laptop cannot run it.

## Runners on your own machines

Not built yet. The plan is single-use runners, started only for private repositories you list, with
no Forgeline secrets and the credential kept in memory only. See [design note 011](https://github.com/forgelinehq/forgeline/blob/main/docs/design/011-github-actions.md).

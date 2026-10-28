# 011: GitHub Actions from Forgeline

Status: **Slices 1 and 2 built** (the Actions page, and the workflow step). Slice 3 (runners) is designed here, not built. Date: 2026-10-28.

## Question

People who keep their CI in GitHub Actions should be able to start, watch and re-run it from
Forgeline. Some also need the jobs to run on their own machines (private networks, cost). What does
Forgeline do, and what keeps it from becoming a way to run strangers' code on the host?

## Decisions

| Decision | Choice |
| --- | --- |
| Slice 1 | **Control Actions through the API.** List workflows and runs, start a workflow (`workflow_dispatch`), cancel, re-run, re-run failed jobs, read a job's log. Nothing runs on the Forgeline host |
| Which repositories | Only those on the GitHub allow list (the same list that decides which pushes may start Forgeline runs) |
| Which branches | A workflow can only be started on a branch the repository's rule allows. Starting a workflow runs that branch's workflow file, so the ref is as trusted as a push to it |
| Who | Developer and admin. Viewers cannot see GitHub data. Writes (start, cancel, re-run) are limited to 20 a minute per person and audited, with refusals |
| Logs | The server fetches the end of the log (256 KB) and returns it as text. GitHub answers with a redirect to a short-lived download address; the server follows it by hand, **without the GitHub token**, only to a web address (no credentials in it, https unless the API itself is loopback, no bare IP number) |
| Text from GitHub | Run names, branches, log lines and links are untrusted. The page inserts them as text. A link is shown only if the server saw it was http(s) with a host |
| Forks | A run whose code comes from another repository is marked `fork`, with a warning in its detail |
| Missing permission | GitHub's "not accessible by integration" is turned into "the GitHub App does not have the Actions permission yet" |

## Permissions

The GitHub App `forgeline-connect` was created with contents read and commit statuses write. Slice 1
needs **Actions: read and write** on the App. GitHub asks the App's owner to approve a permission
increase on the App's page, so this is a step for a person, not something Forgeline can grant itself.
The page works with a personal token too, if the token has the `repo` scope (classic) or the Actions
permission (fine-grained).

## Slice 2 (built): a step that starts a workflow

```yaml
- id: ci
  workflow: {repo: acme/widgets, workflow: ci.yml, ref: main, inputs: {env: staging}}   # detach: true to not wait
```

It runs on the Forgeline server, not in a container, because it needs the GitHub credentials and
those never enter a container. So the step has no image, script, environment or permissions.

- It starts the workflow, finds the run it started (GitHub's answer names it when it can; otherwise
  the run that was not there before), polls until it ends, and succeeds only if the conclusion is
  `success`. `timeout:` limits the wait.
- If the Forgeline run is canceled, or the step times out, the GitHub run is canceled too, so work
  this server started does not carry on unseen.
- The same rules as the Actions page are checked again **when the step runs**: the repository must
  be on the allow list, the branch must be one its rule allows, inputs are bounded, and starts share
  the 20 a minute limit. A pipeline saved earlier cannot outlive a change to the allow list.
- **Runs started by GitHub cannot use it** (a push or pull request controls the pipeline, and the
  step would start more work with Forgeline's credentials). Like secrets, this stays refused until
  runs have trust levels (note 005, slice 3).
- Each start and each refusal is in the audit log under the actor `run <id>`.
- `forgeline run` on a laptop cannot run it and says so.

## Slice 3 (not built): Forgeline-managed self-hosted runners

GitHub's hosted runners cost minutes and cannot reach private networks. Forgeline would start runners
on its own host. A runner is a credential holder that executes whatever a workflow says, so these are
the rules before any code is written.

1. **Just-in-time (JIT) configuration only.** Use `POST /repos/{o}/{r}/actions/runners/generate-jitconfig`.
   A JIT runner is single use: it takes one job and GitHub removes it. Never use a long-lived
   registration token (`…/registration-token`): it can register persistent runners.
2. **The JIT configuration is a secret.** It is requested only when a job is queued, held in memory,
   passed to the container through its environment (not on a command line, where `ps` shows it), never
   written to disk, the audit log or a log, and masked if it ever appears in output. It is not stored
   after the container starts.
3. **One job, one container.** A fresh container per job, removed afterwards, with a time limit. No
   Docker socket, no host mounts, no privileged mode, dropped capabilities, limits on memory, CPU and
   processes, and a network policy chosen by the administrator (default: internet only, not the
   Forgeline host or private ranges).
4. **No Forgeline secrets.** The runner never receives the vault, the server token or cloud
   credentials. A workflow's own secrets come from GitHub.
5. **Never for untrusted code.** Runners are created only for repositories the administrator lists for
   runners, **private repositories only** unless an explicit opt-in is ticked for a public one, and the
   console says why. A job from a fork is never given one. This is the same rule as for Forgeline's own
   runs: a self-hosted runner on a public repository can be attacked through a pull request.
6. **Routing by label.** Runners get a label unique to Forgeline (`forgeline`). Forgeline starts one
   only for a queued job that asks for that label, in an allowed repository, from a `workflow_job`
   webhook whose signature is valid.
7. **Limits.** A maximum number of runners at a time, and a rate limit on JIT requests. GitHub returns
   409 or 422 if a repository is hammered; those are surfaced, not retried in a loop.
8. **Visible and revocable.** An Infrastructure page lists runners (from GitHub and from Docker) and
   can remove one (`DELETE …/actions/runners/{id}`) and stop its container.
9. **Permission.** Repository runner administration needs a broad App permission
   ("Administration: read and write", to be confirmed against a real App). The console would show it
   as a separate, optional step that an administrator approves, because it is much wider than Actions.

## What was checked

Go tests with a stand-in GitHub, each rule broken one at a time to prove a test fails: the allow list,
the branch rule, the token kept off the download address, the link scheme, bare IP and plain http
refusals, the rate limit, workflow and input checks, and the log tail. A real browser walk-through
against `scripts/dev/fake-github.py` (a pipeline with a container step and a workflow step, built from the step builder, run and watched to the end; start with an input, watch queued, running and succeeded,
re-run, cancel, read a failed run's log, a refused branch).

**Not checked:** the real GitHub API with the App (the App needs the Actions permission first).

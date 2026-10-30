# Changelog

All notable changes to Forgeline. The format follows [Keep a Changelog](https://keepachangelog.com/),
and versions follow [Semantic Versioning](docs/guides/versioning.md). Change the `Unreleased`
section in the pull request that makes the change; a release moves it under its version and date.

## [Unreleased]

### Added
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

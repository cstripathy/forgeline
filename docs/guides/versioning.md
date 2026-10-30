---
title: Versioning
parent: Guides
nav_order: 8
---

# Versioning

Forgeline versions three different things. They move independently.

## The product

Releases follow [Semantic Versioning](https://semver.org/): `MAJOR.MINOR.PATCH`.

- **PATCH**: fixes, no behaviour change you would notice.
- **MINOR**: new features, new API routes, new optional fields. Existing pipelines and clients keep working.
- **MAJOR**: anything that breaks a pipeline file, the HTTP API or stored data.

Until 1.0.0, a MINOR release may include a breaking change, and the changelog says so under
**Changed**. `forgeline version` and the Platform tab in the console show the running version.
A build between tags shows `v1.2.3-4-gabc123`, meaning four commits after `v1.2.3`; `-dirty`
means it was built with uncommitted changes.

### Cutting a release

1. Move the `Unreleased` section of `CHANGELOG.md` under a new heading with the version and date.
2. Merge that to `main`, then tag it: `git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0`.
3. The `release` workflow tests, builds binaries for Linux, macOS and Windows (amd64 and arm64),
   writes `SHA256SUMS`, and publishes a GitHub release with the changelog section as its notes.

## The HTTP API

Routes live under `/v1`. Within `/v1` we add routes and optional fields but do not remove or change
the meaning of existing ones. A breaking change would be `/v2` served alongside `/v1` for at least
one MINOR release. `docs/api/reference.md` lists every route and a test fails if one is missing.

## Skills and agents

Each skill and agent keeps every version it has ever had:

- A save creates the next version. Old versions are never edited.
- Each version has a fingerprint (a SHA-256 of its content), who saved it, when, and a note.
- An agent version **pins** the exact skill versions it uses, so the same agent version behaves the
  same way later even after a skill changes. The agent page shows when a newer skill version exists;
  saving a new agent version takes it.
- **Rollback** saves a copy of an old version as the newest one, so the history only grows.
- A run records which agent version and fingerprint answered, in the audit log and the reply.
- An agent version cannot gain a tool or a higher tier without a new saved version, which is audited.

Marketplace packages (planned) will use the same rules plus a pinned hash, so an update can never
change what is already installed without someone choosing it.

# 002: Spec extensions for caching and the trust model

Status: **Proposed.** Date: 2026-10-21. Only `inputs` is implemented. Everything else here
is reserved or deferred, and says so.

## Question

Step caching (M1 step 6) needs steps to declare what they read. The trust model (M3) needs
steps to declare what they may do. Which fields does the spec add now, and which are only
reserved so later milestones do not break the format?

## Implemented now: `inputs`

```yaml
steps:
  - id: test
    image: golang:1.27
    run: go test ./...
    inputs: ["go.mod", "**/*.go"]
```

- `inputs` is an optional list of glob patterns, relative to the workspace root.
- Syntax: `/`-separated; `*`, `?` and `[...]` match within one path segment (as in Go's
  `path.Match`); `**` as a whole segment matches zero or more segments; a pattern naming a
  directory matches every file beneath it.
- Validation rejects an empty pattern, an absolute pattern, a `..` segment, a backslash,
  and a malformed bracket expression. A pattern that matches no files is allowed: it hashes
  as "no files".
- **A step with no `inputs` is never cached.** Caching a step whose reads are undeclared is
  unsound, so the safe default is to run it every time.

## Cache key

A step's key is a SHA-256 over, in this order, with each field length-prefixed:

1. A cache format version and the spec version.
2. The image reference exactly as written.
3. The script, or the command and arguments.
4. The environment, sorted by name.
5. The working directory (empty and `/workspace` are treated as the same).
6. For each file matched by `inputs`, in sorted path order: its relative path and the
   SHA-256 of its contents.

The key is computed **just before the step would run**, not up front. An upstream step may
change files in the workspace, and a downstream step's inputs must reflect that. A step is
cached when the stored record for its key records a success.

Known limits, stated rather than hidden:

- Image tags are mutable. `golang:1.27` can change under the same key. Resolving to a
  digest is deferred; pin a digest in `image` to get exact behaviour.
- File mode and symlink targets are not hashed in M1. Only paths and contents are.
- Anything a step reads outside its declared `inputs` (network, files in the image, time)
  is invisible to the key.
- Env values are hashed, not stored. Do not put secrets in `env`; M3 adds a proper path.
- Only success is recorded. Cached steps do not replay logs in M1.

## Deferred: `outputs`

Not added. The M1 sample pipeline passes data between steps through the shared workspace,
so declared outputs are not needed. Revisit with artifact storage in M2.

## Permissions: one field enforced, the rest reserved for M3

The threat model is in [005](005-threat-model.md). Only a permission that is enforced is added
to the types; the loader rejects unknown fields, so a reserved name fails to load. That is
intentional: a permission that parses but is ignored would give false confidence.

```yaml
permissions:
  network: none|egress   # IMPLEMENTED (M3 slice 1): none runs the step with no network at all
  secrets: [NAME, ...]   # reserved: secrets the step may read
  untrusted_input: false # reserved: whether the step may receive untrusted input
```

`network: none` is enforced with Docker's `none` network and is covered by tests that try TCP,
DNS, HTTP, the bridge gateway and the cloud metadata address from inside a real container.
**The default when `network` is omitted is `egress`**, which keeps existing pipelines working.
An earlier sketch of this note said the default would be `none`; whether to flip it is an open
decision in note 005, and the spec is alpha, so it is cheapest to change now. The setting is
part of the cache key.

Intended rule, from the trust model: a step must never combine untrusted input, secrets, and
external actions unless each is declared and granted. Secrets and the combination rule are
slices 2 and 3 in note 005 and are **not enforced yet**.

## What is implemented versus reserved

| Item | State |
| --- | --- |
| `steps[].inputs` and its validation | Implemented in the Go types and generated schema |
| Cache key definition above | Defined here, implemented in M1 step 6 |
| `outputs` | Deferred |
| `permissions.network` | Implemented and enforced for `none` (M3 slice 1) |
| `permissions.secrets`, `permissions.untrusted_input` | Reserved, rejected by the loader, not enforced |

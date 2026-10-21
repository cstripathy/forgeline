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

## Reserved for M3, not implemented, not enforced

These names are reserved. The loader rejects unknown fields, so a pipeline that uses them
fails to load today. That is intentional: a permission that parses but is ignored would give
false confidence.

```yaml
permissions:            # reserved
  secrets: [NAME, ...]  # secrets the step may read
  network: none|egress  # default would be none
  untrusted_input: false # whether the step may receive untrusted input
```

Intended rule, from the trust model: a step must never combine untrusted input, secrets, and
external actions unless each is declared and granted. Enforcement, defaults, and the
threat model come with M3. Until then Forgeline provides **no** permission enforcement.

## What is implemented versus reserved

| Item | State |
| --- | --- |
| `steps[].inputs` and its validation | Implemented in the Go types and generated schema |
| Cache key definition above | Defined here, implemented in M1 step 6 |
| `outputs` | Deferred |
| `permissions.*` | Reserved, rejected by the loader, not enforced |

# 019: Signed packages from a git source, and workflow and environment-template packages

Status: built. Extends 017 slice 7 (marketplace) and 018 (plugins).

## Why

The marketplace ships packages in the binary and, for an operator, from a local directory
(`FORGELINE_MARKET_DIR`). Third parties cannot publish packages without a new Forgeline release or
access to the server's disk. A git repository is the natural place for them, but a repository is only as
trustworthy as whoever can push to it. So a package from git is shown only if it is signed by a key the
operator chose to trust, and nothing from the repository is ever executed.

## The git source

The operator sets, in the server's environment (never from the console):

- `FORGELINE_MARKET_GIT`: an `https://` URL, a `file://` URL or an absolute path. No credentials in the URL
  (it would show up in process lists); public repositories only.
- `FORGELINE_MARKET_GIT_REF`: branch or tag, default `main`.
- `FORGELINE_MARKET_KEYS` (or `FORGELINE_MARKET_KEYS_FILE`): trusted ed25519 public keys as
  `id=base64` entries separated by commas, semicolons or newlines. A key id is a lowercase slug.

The server fetches `packages/*.json` and `packages/*.sig` with `git fetch --depth 1` into a bare
repository under its data directory, and reads them with `git ls-tree` and `git cat-file`. There is no
checkout, so there are no hooks, no filters, no submodules and no symlinks to follow. Only regular blobs
directly under `packages/`, at most 256 KB each and 400 in all, are read. Git runs with an empty
environment apart from `PATH`, no global or system configuration, no prompts, and only the `https` and
`file` transports. The fetch runs at start, then every hour, and when an administrator presses refresh.
A failed fetch keeps the last good snapshot and shows its error.

## What is signed

`packages/<name>.json` is the manifest; `packages/<name>.sig` is `{"key_id": "...", "signature": "<base64>"}`.
The signature is ed25519 over `forgeline-market-v1:` followed by the manifest's content hash (the same
sha256 over the canonical JSON with the hash field blanked that the other sources already use). The hash
covers the package id, version and everything installable, so a signature cannot be moved to other
content, another id or another version. Manifests from git are parsed strictly (unknown fields are an
error), so nothing outside the signed form can be in the file.

A package is listed as installable only if all of these hold. Otherwise it is listed as damaged with the
reason, and cannot be installed:

1. the `.sig` exists and parses;
2. `key_id` names a key in the operator's list (a removed key un-trusts everything it signed);
3. the signature verifies over the hash;
4. the content passes the same check as any package, hash included;
5. the version is not lower than the highest version this server has already verified for that id from
   git (rollback), and a version already seen has the same hash it had (equivocation).

The highest version seen is stored when a snapshot is verified. This is the replay defence: an attacker who
can write to the repository, or serve an old tag, cannot hand back an old signed package that was
since fixed.

An install records its source and signer. An upgrade must come from the same source and signer, so a
second trusted key cannot take over a package id, and a package can never shadow a built-in one (the
duplicate-id rule from 017 still applies, built-in first).

Not covered: a trusted key that is stolen can sign anything until the operator removes it from the list;
there is no online revocation list, no key rotation protocol (add the new key, remove the old one) and no
transparency log.

### Publishing

`forgeline market keygen NAME` writes `NAME.key` (a seed, mode 0600) and prints the public key line to
put in `FORGELINE_MARKET_KEYS`. `forgeline market sign -key NAME.key -id NAME manifest.json` fills in
the sha256 and writes `manifest.sig` beside it.

## Workflow packages

A package may hold `workflows`: stages of steps, each step naming an agent **by the key of an agent in the
same package** and giving its task. At install the keys become the ids of the agents the install made, and
the result goes through the same checks as a workflow made by hand. A package can neither schedule a
workflow (`every_minutes` is not in the package form) nor reference an agent it does not ship. Gates are
kept. `run_as` is the most the workflow may run as: at install tier T0 it is lowered to viewer. Upgrade adds
a version when the content changed; uninstall turns the workflow off and keeps its history.

## Environment-template packages

A package may hold `env_templates`: a CloudFormation body, its parameters, a default time to live and a
monthly cost estimate (the publisher's claim, shown as such). They become catalog templates named
`pkg.<package>.<key>`. Requesting an environment from one is the ordinary path: preview as a change set,
four-eyes approval. In addition:

- a third-party template can never be on the self-service list, so it always needs an administrator;
- the body is at most 50 KB and must not contain IAM resources, nested stacks, custom resources, macros
  or `Transform`, and no capability is ever passed, so CloudFormation itself refuses what slips through;
- it must declare `Owner` and `ExpiresAt` parameters (the server sets them);
- the estimate and the parameters are shown in the review before install.

Uninstall removes the template from the catalog; environments already created keep running and are cleaned
up by the usual expiry. Upgrade replaces the template for new requests only.

## Not built

Plugin-bearing packages from git are allowed (the grant flow of 018 is unchanged), but the image digest is
only as trustworthy as the signer. There is no per-publisher allow list of package ids, so a trusted key can
publish any id not already taken by a built-in or local package.

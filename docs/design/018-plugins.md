# 018 Plugins: MCP servers in containers

Status: **built** (steps 1 to 4). Pictures: [54](../console/54-marketplace-plugin.png). Follows note 017 slice 7 (marketplace). A plugin gives the assistant
and agents new read-only lookups from a third party's MCP server. It is the one place Forgeline would
run code it did not write, so the rules here are strict and the first version is small.

## What a plugin is

An MCP server shipped as a container image, listed in a marketplace package next to skills and
agents. The package names the image **by digest** (`registry/name@sha256:...`), never a tag, and the
package's own sha256 covers that digest. Installing a plugin is the same reviewed, hash-bound,
administrator-only install as any package, with an extra permissions screen (below).

## Decisions

| Decision | Choice |
| --- | --- |
| Where it runs | Never in the server process. One container per call, started with `docker run --rm -i` and spoken to over stdio through `internal/mcp`, like the AWS proxy. No long-lived processes, nothing to keep patched or exposed |
| Container flags | Fixed by Forgeline, not by the manifest: `--read-only`, `--cap-drop ALL`, `--security-opt no-new-privileges`, non-root user, `--pids-limit`, `--memory`, `--cpus`, a short timeout, no volumes, no host mounts, no Docker socket, a throwaway `tmpfs` for `/tmp` |
| Network | `--network none` unless the manifest asks for egress **and** an administrator allows it at install. Version one has only two states: none, or open egress with a plain warning. A per-host allowlist needs an egress proxy and is later work |
| Credentials | None by default. A plugin may name secrets it wants; the administrator picks which vault secrets to give it at install. They are passed as environment variables for that call only. A secret value is never shown to the assistant and never put in a prompt |
| What it can do | Read only. Its tools become lookups named `plugin_<id>_<tool>`, role developer and up, and an agent must list one to use it. A plugin cannot call Forgeline's API, file a proposal or reach another plugin. It can only return text |
| Its output | Untrusted data: size-capped, escaped the way lookup results are, and wrapped so the model is told it is data. A plugin that returns instructions cannot give the model new permissions, because permissions come from the agent's tool list and the asker's role, not from text |
| Arguments | The model's arguments are checked against the tool's JSON schema from the manifest before the container starts. Unknown fields are refused |
| Image trust | Digest-pinned. The server pulls on install and refuses a digest that does not match what the manifest says. Updating a package that changes the digest, the network setting or the secrets it wants shows the difference and needs a fresh approval; it never keeps the old grant silently |
| Switch | Off by default and only offered when Docker is reachable. Administrators turn it on under Settings; turning it on runs a self-test container and reports the result |
| Audit | `plugin.install`, `plugin.upgrade`, `plugin.uninstall`, and every call: plugin id, tool, asker, agent if any, exit status, bytes returned. Arguments are logged truncated; results are not stored |

## Permissions screen at install

Names the image digest, whether it wants network and what for, which secrets it asks for, the tools it
adds with their arguments, and the limits it runs under. The administrator ticks each grant; nothing
is on by default.

## Threats considered

- **Malicious image**: cannot touch the host (no mounts, no socket, no capabilities, read-only, no
  network by default). Worst case with granted egress and a secret is that it leaks that one secret,
  which is why a secret is a separate, visible grant.
- **Prompt injection through output**: output is data. The most it can do is mislead an answer; it
  cannot widen a tool list or role.
- **Argument injection**: arguments never reach a shell; they go over the MCP protocol as JSON.
- **Resource abuse**: per-call memory, cpu, pids and time limits; one plugin call at a time per plugin.
- **Supply chain swap**: digest pinning plus the package hash; a changed digest needs approval.
- **Container escape**: not defended beyond Docker's defaults and the flags above. Say so in the
  docs; recommend running Forgeline's Docker on a host that has nothing else on it.

## Build plan

1. (built) `internal/plugin`: build the `docker run` argv from a fixed template, run one call, enforce the
   timeout and output cap. Tested with a fake `docker` script that records its argv, so the flags are
   asserted exactly; one real run with a tiny image behind a skipped-if-no-Docker test.
2. (built) Manifest and package kind `plugin`; the install flow and permissions screen; grants on the install
   record.
3. (built) Lookups `plugin_<id>_<tool>` registered from installed plugins, with schema checks, agent
   allowlisting, role checks, output wrapping.
4. (built, as a dev fixture) A sample plugin (a read-only example) so the whole path is exercised end to end.

## Not decided

- Egress allowlists by host (needs a proxy container).
- Long-lived plugin processes for slow-starting servers.
- Signed manifests from third parties (the git source in note 017).

## What was built, and how it was checked

- A package may carry `plugins`: image, whether it wants network, the environment variables it
  wants, and each tool with an argument schema. The package sha256 covers all of it.
- Plugins are off until an administrator turns them on (needs Docker to answer). Install shows the
  image, tools and asks, and grants nothing by default: network and each secret are separate
  choices. The image is pulled by digest (or present locally by id) and started once to check it
  really offers the listed tools.
- An update that changes the image, the network ask or the secret ask needs a fresh grant (409
  otherwise); an update that changes none of them keeps the old grant.
- Tools appear as lookups `plugin_<package>_<plugin>_<tool>`, developer and up, usable by an agent
  only when it lists them. Arguments are checked against the schema (unknown, missing, wrongly typed
  or too long arguments are refused before any container starts). Output is returned as untrusted data.
- Tried for real: `scripts/dev/sample-plugin` is a static Go MCP server in a `FROM scratch` image.
  In real Docker it ran as uid 65534 with a read-only root, no network, and the granted secret, and a
  real `claude` agent used both of its tools through the console. The secret value appears in no
  audit entry and no log, and no container was left behind.
- `FORGELINE_MARKET_DIR` adds a directory of manifests as another source (checked the same way,
  hash included). It is set by whoever runs the server, never by the console. A duplicate id is shown
  as a damaged "duplicate" entry and cannot stand in for the first package.

Not built: egress allowlists, long-lived plugins, signed third-party manifests, a published plugin
image (the sample is built locally and referenced by its image id).

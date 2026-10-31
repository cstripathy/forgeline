# 021 Per-account writes and plugin egress allowlists

Status: part A (per-account writes) **built**; part B (plugin egress allowlists) planned in this note, built in its own pull request. Follows 017 (platform roadmap), 016 (AWS MCP Server) and 018 (plugins).

## A. Per-account writes

Today a change can be proposed for the primary AWS connection only; added accounts are read-only.

| Decision | Choice |
| --- | --- |
| Opt-in | Each added account has a `writes` switch, **off by default**, that only an administrator can turn on (`PATCH /v1/aws/accounts/{id}`). A developer cannot widen where changes may go. Turning it off, or removing the account, cancels every proposal still waiting for that account |
| Which account | `proposeChange` (cloud changes) and the assistant's `propose_aws_change` take an optional `account` id. Empty means primary, as before |
| One proxy per account | The AWS MCP signing proxy is started per call with that account's profile and region (it already was per call; the settings are now passed in, not read from the primary). The primary's SSO session is never used for another account |
| Binding | A proposal records the account id, its AWS account number (proven with `sts get-caller-identity` when it was prepared), profile and region. Approval re-resolves the account, refuses if it was removed, writes were switched off, or profile, region or account number differ, and (for AI proposals) the hash covers the target, so changing it changes the hash the approver signs |
| Visibility | The approver sees the account name and number on the proposal. Titles and the audit log name the account. A change against account X can never run against Y |
| One at a time | The "one change per target" lock includes the account, so the same stack name in two accounts does not collide, and the same resource in one account still does |
| Unchanged | Four-eyes, expiry, the script checks (no delete/terminate/IAM), previews, environments (primary only), Guard and the explorer |

Not covered: cross-account roles or assume-role chains (an account is a profile on the server), and a
policy that restricts which developers may propose to which account (only the per-account switch).

## B. Plugin network allowlists by host

Today a plugin gets `--network none` or the whole network. Planned: an install grant can name hosts.

| Decision | Choice |
| --- | --- |
| Enforcement | By topology, not by the plugin's cooperation. Each call that has an allowlist gets its own `docker network create --internal` network (no route out) and the container is attached only to it. The only reachable address is the network's gateway, where Forgeline listens for that call with a small forward proxy. A plugin that ignores proxy settings reaches nothing |
| The proxy | HTTP CONNECT and plain HTTP forwarding. It checks the requested host against the grant (exact name or `*.suffix`), allows only listed ports (default 443), resolves the name itself, **refuses loopback, private, link-local, metadata and multicast addresses**, dials the address it resolved (no second lookup), caps concurrent tunnels, bytes and time, and counts allowed and denied hosts into the audit entry for the call |
| No DNS | The container has no resolver that reaches the outside; names are resolved by the proxy only |
| Fail closed | If Forgeline cannot listen on the gateway (for example it runs inside a container itself), the call is refused with that reason. It never falls back to open network |
| Grant | `network` becomes `none`, `hosts` or `open`. `hosts` carries the list. The manifest says which hosts it needs; the administrator grants all, some, or none. A changed host list in an update needs a fresh grant, like a changed image |
| Guarantees | The container can open TCP only to the proxy. The proxy tunnels only to allowed names on allowed ports and only to public addresses |
| Does not guarantee | What an allowed host does with data (a CDN or relay host can forward it); TLS is tunnelled, not inspected, so an allowed host name is trusted by name only (domain fronting through an allowed shared host is possible); UDP, ICMP and raw IP are blocked by topology but not logged; a host kernel or Docker flaw still escapes everything |

## Build plan

1. Part A, one pull request: `writes` switch, account on proposals, per-account proxy, binding and
   invalidation, UI, tests (including mutating each rule).
2. Part B, one pull request: grant model, proxy package, per-call network lifecycle, tests against a
   real Docker daemon, UI.

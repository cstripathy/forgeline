# Architecture (intended, not yet built)

This document describes the intended shape of Forgeline. Everything below is a plan.
Decisions marked **open** are not settled.

## Components

- **Control plane** (Go). A stateless service exposing an API, storing state in Postgres,
  with a queue for runs. All configuration lives in Git, not in the server.
- **Agents/runners** (Go). Stateless binaries that execute steps in containers, on
  Kubernetes (ephemeral pods) or on VMs.
- **Pipeline SDK.** Typed pipelines in TypeScript, Python, and Go that compile to one
  model, plus a YAML form. The same pipeline runs locally and remotely.
- **UI.** A web interface showing the run DAG, live logs, and a diff view for config changes.
- **MCP surface.** The control plane exposes its operations to agents over MCP, under the
  calling user's permissions.

## Trust model (the core differentiator)

A job is risky when it has all three of: untrusted input (pull request text, issue bodies,
branch names), access to secrets, and the ability to act externally. Forgeline's rule:

1. Every step declares the inputs, secrets, network access, and tools it needs.
2. The platform grants only what is declared and policy allows.
3. Agent decision-making is separated from the layer that holds credentials.
4. Credentials are short-lived and scoped to a step.
5. Every grant and every AI action is written to an audit log.

The model gets a written threat model and adversarial tests in milestone M3.

## Execution

Steps run as OCI containers. **Open:** build on an existing engine or write a minimal one.

## State and orchestration

**Open:** start with a Postgres-backed DAG executor and keep the engine behind an
interface so a durable-execution engine can be adopted later.

## Delivery

One declaration per strategy (canary or blue-green) translated per target. Kubernetes
first, with traffic shifting through the Gateway API or a rollout controller and
metric-driven rollback. Other targets come after.

## Extensibility

No in-process plugins. Container steps first. **Open:** WASM components for small,
sandboxed hooks, once the component model's language support is mature enough.

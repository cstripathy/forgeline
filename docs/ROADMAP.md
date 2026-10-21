# Roadmap

Each milestone has exit criteria. A milestone is done only when its criteria are met and
demonstrated, not when the code exists. The order puts the trust model early because it is
the project's differentiator.

M1 progress: step 1 (model, JSON Schema, validation) is done. The remaining steps, with
acceptance criteria, are in [PLAN.md](PLAN.md).

| # | Milestone | Exit criteria | Status |
| --- | --- | --- | --- |
| M0 | Foundation | Public repo, Apache-2.0 license, README, roadmap, architecture notes, Go skeleton that builds and tests locally | Done |
| M1 | Pipeline spec and local engine | Typed pipeline schema; YAML loader; DAG executor running container steps; `forgeline run` executes a sample pipeline on a laptop with caching of unchanged steps | In progress |
| M2 | Control plane and UI | Single-binary server with Postgres; run history and live logs; GitHub webhook trigger with commit status; a DAG run view in the UI | Not started |
| M3 | Trust and sandbox model | Per-step declared permissions; secrets scoped to steps; untrusted input isolated from secrets; audit log; a published threat model with tests that try to break it | Not started |
| M4 | Security gates | Scanner orchestration; findings normalized from SARIF; policy gates with expiring waivers; SBOM and image signing | Not started |
| M5 | Progressive delivery | One declaration for canary and blue-green on Kubernetes with metric-driven rollback | Not started |
| M6 | AI agents | Governed agent steps with scoped credentials and cost tracking; failure triage; fix suggestions as pull requests, never auto-applied | Not started |
| M7 | Migration and breadth | Importers for Jenkinsfile and GitHub Actions workflows; Bitbucket and GitLab triggers; additional cloud targets | Not started |

## Open decisions

- Step execution engine. Proposed: write a minimal executor behind an interface and treat
  Dagger as an interoperable step type. See [design/001](design/001-execution-engine.md).
- Workflow state: a Postgres-backed executor to start, with the engine kept swappable.
- Policy language for gates (candidates: OPA, CEL). Not yet researched.
- Plugin model beyond container steps. WASM components are a candidate for small policy hooks.
- Verification of project name availability beyond a basic search. Not a trademark clearance.

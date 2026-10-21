# 001: Execution engine for milestone M1

Status: **Proposed.** Date: 2026-10-21. This is a recommendation based on README-level
research, not hands-on evaluation. Nothing here has been benchmarked.

## Question

Should Forgeline write its own step executor, or build on an existing engine?

## Options considered

| Option | License | What it offers | Fit for M1 |
| --- | --- | --- | --- |
| Dagger | Apache-2.0 | CLI plus a language-agnostic API with generated SDKs; depends only on a container runtime; runs locally, in CI, or in its cloud | Presented as a CLI and API, not as an embeddable library, so using it means depending on an external tool |
| BuildKit | Apache-2.0 | LLB graph format; instruction-level caching with several cache backends; rootless mode; gRPC API | Strong for caching and content-addressed steps, but designed for turning source into artifacts. Using it as a general step executor is an inference from its design |
| Argo Workflows | Apache-2.0 | Kubernetes-native workflow engine as CRDs; steps in containers; sequences or DAGs; CI/CD listed as a use case | Requires Kubernetes, which breaks the laptop-first principle |
| Own minimal executor | Apache-2.0 (ours) | Exactly the semantics the trust model needs; no external runtime beyond a container runtime | More code to write and maintain |

## Recommendation

1. **Write a minimal executor for M1**, behind an `Executor` interface, running each step as
   a container through the local container runtime. The trust model in M3 needs control over
   per-step permissions, secret scoping, and input isolation, which is hard to guarantee
   through someone else's engine.
2. **Treat Dagger as an interoperable step type**, not a foundation. A pipeline step can call
   a Dagger function. This keeps users who already have Dagger modules, without making
   Forgeline depend on another company's roadmap. Earthly's shutdown is the cautionary example.
3. **Revisit BuildKit in M2 or later for caching**, once the executor interface and the
   cache-key model are settled.
4. **Add a Kubernetes backend** to the same `Executor` interface for cluster runs, using
   ephemeral pods. Argo Workflows is not required for that.

## What is not verified

- The language of Dagger's engine and whether any part of it can be embedded as a library.
  The README does not say.
- Real-world behavior and overhead of BuildKit as a general step executor.
- Whether each option's maintenance burden matches the assumptions above.

A short hands-on spike for Dagger interop and BuildKit caching should precede any change to
this recommendation.

## Consequences

- M1 includes writing the executor, so it is larger than adopting an engine.
- The `Executor` interface becomes a stable internal boundary and the first candidate
  for a written spec.

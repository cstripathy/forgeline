---
title: Write pipelines
parent: Guides
nav_order: 2
---

# Write pipelines

A pipeline is a list of steps. Each step is a container image plus one thing to do. Run
`forgeline schema` for the exact JSON Schema; most editors can use it for completion.

```yaml
version: v1alpha1
name: build-and-test
steps:
  - id: build
    image: golang:1.27
    run: go build ./...
    inputs: [go.mod, go.sum, "**/*.go"]

  - id: test
    image: golang:1.27
    run: go test ./...
    needs: [build]

  - id: publish
    image: alpine:3.24
    run: ./publish.sh
    needs: [test]
    timeout: 5m
    permissions:
      network: egress
      secrets: [REGISTRY_TOKEN]
```

## Step fields

| Field | Meaning |
| --- | --- |
| `id` | Unique name inside the pipeline. |
| `image` | Container image. Required. |
| `run` / `command` / `agent` | **Exactly one.** `run` is a shell script, `command` an argument list with no shell, `agent` a coding agent. |
| `needs` | Steps that must succeed first. Steps with no dependency between them run in parallel. |
| `inputs` | Files the step reads. If none changed since the last good run, the step is served from the cache (`--no-cache` turns that off). |
| `env`, `workdir`, `timeout` | Environment, working directory, and a time limit such as `10m`. |
| `permissions.network` | `none` cuts the step off from every network; `egress` allows outbound access. |
| `permissions.secrets` | Names of secrets this step may read. A step gets nothing it does not list. |

## Secrets
Add one under **Secrets** (name in capitals, value of at least 8 characters). Request it with
`permissions.secrets`; it arrives as an environment variable of the same name and is masked as
`***` in stored output.

![A secret used with no network]({{ '/console/32-vault-secret-run.png' | relative_url }})

Runs **started by GitHub get no secrets** yet; runs started by a signed-in developer or admin do.

## From the command line

```sh
forgeline run pipeline.yaml                                # locally in Docker
forgeline submit --server URL --wait pipeline.yaml         # to a server (uses FORGELINE_TOKEN)
```

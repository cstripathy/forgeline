---
title: Get started
nav_order: 2
---

# Get started

## What you need

- Go 1.27 or newer (to build) and Docker (to run steps). Linux, macOS, or Windows with WSL2.
- A browser.

## 1. Build and start

```sh
git clone https://github.com/forgelinehq/forgeline && cd forgeline
make run-local
```

`make run-local` builds the binary and starts the server on `127.0.0.1:8080`. On a loopback
address with no `FORGELINE_TOKEN` set, it makes a random token and **prints it**. Keep it: you
need it once, for setup.

To keep the same token across restarts, and rebuild and restart in one go:

```sh
scripts/dev/demo.sh                 # data in /tmp/fl-demo, http://127.0.0.1:8080
scripts/dev/demo.sh ~/forgeline-data 127.0.0.1:9000
```

The token is saved in `<data-dir>/.token` (mode 0600).

## 2. Create the first administrator

Open <http://127.0.0.1:8080>. The first visit shows the setup form. Paste the server token,
choose a username and a password of at least 12 characters.

![First run]({{ '/console/01-setup.png' | relative_url }})

The token is required so that whoever reaches the port first cannot take the server over.
After setup you sign in with the username and password; the token keeps working for the CLI and the API.

## 3. Run a pipeline

Go to **Pipelines → New pipeline** and paste:

```yaml
version: v1alpha1
name: hello
steps:
  - id: greet
    image: alpine:3.24
    run: echo "hello from Forgeline"
```

The editor checks it as you type. **Save**, then **Run**. You see the steps as a graph, coloured by
status, with live logs.

![A run in progress]({{ '/console/05-run-live.png' | relative_url }})

Prefer the command line? These work without the console:

```sh
forgeline run examples/hello/pipeline.yaml                # on your machine, in Docker
forgeline submit --server http://127.0.0.1:8080 --wait examples/hello/pipeline.yaml
forgeline schema                                          # the JSON Schema, for editor support
```

## 4. Next

- Put values a step needs in **Secrets**, then ask for them with `permissions.secrets`: [Pipelines]({{ '/guides/pipelines/' | relative_url }}).
- [Connect GitHub]({{ '/guides/connect-github/' | relative_url }}) so a push runs your pipeline.
- [Connect AWS]({{ '/guides/connect-aws/' | relative_url }}) to see and change your cloud.
- Turn on the [assistant]({{ '/guides/assistant-and-agents/' | relative_url }}).

## Server settings

| Variable | Meaning |
| --- | --- |
| `FORGELINE_TOKEN` | Bearer token for the CLI and API, at least 16 characters. Required unless the address is loopback. |
| `FORGELINE_DATABASE_URL` | Postgres URL. Without it, runs live in memory and accounts, pipelines and settings are kept in `<data-dir>/console.json`. |
| `FORGELINE_MASTER_KEY` | 32 random bytes, base64 (`head -c 32 /dev/urandom \| base64`), that encrypt stored secrets. Without it a key file is created in the data directory. |
| `FORGELINE_SECRET_<NAME>` | A secret steps can request without using the vault. |

Flags: `--listen`, `--data-dir`, `--max-runs`, `--parallel`, `--public-url`, `--github-allow`.
Run `forgeline server -h` for the full text.

{: .important }
The server speaks plain HTTP. Put it behind TLS (a reverse proxy or a tunnel) before exposing it, and run it with an AWS role that can do only what you want the console to be able to do.

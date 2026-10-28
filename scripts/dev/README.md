# Development stand-ins

Throwaway helpers for trying the console without touching real accounts. The Go tests do not use
them (they have their own fakes). Never expose them to a network.

## `fake-aws`: a stateful stand-in for the AWS CLI

Implements only the calls the console makes (`sts`, `cloudformation`, `ec2`, `ce`); stacks take a
few polls to finish. Put it first on `PATH` under the name `aws` and run a server:

```sh
mkdir -p /tmp/fake-aws/bin && ln -sf "$PWD/scripts/dev/fake-aws" /tmp/fake-aws/bin/aws
PATH=/tmp/fake-aws/bin:$PATH FAKE_AWS_STATE=/tmp/fake-aws/state.json \
  FORGELINE_TOKEN=a-long-dev-token-123 ./bin/forgeline server --listen 127.0.0.1:8081 --data-dir /tmp/fl-sim
```

In the console create the administrator, a second administrator and a developer, connect AWS
(leave the profile empty, any region), and walk a proposal through approval. Delete the state
file to start over.

## `fake-github.py`: a stand-in for the GitHub API

```sh
python3 scripts/dev/fake-github.py 9911 /tmp/statuses.log
```

It accepts the token `ghp_demo_token_value_1234`, knows one repository (`acme/shop`) whose commit
holds a small `forgeline.yaml`, and appends every commit status it receives to the log. In the
console set the API address to `http://127.0.0.1:9911` (plain http is allowed only for loopback),
the token to that value and the repository to `acme/shop`; saving shows a webhook secret once. Then
send a signed `push` event: the header `X-Hub-Signature-256: sha256=<HMAC-SHA256 of the body with
the secret>`, `X-GitHub-Event: push`, and a body like
`{"ref":"refs/heads/main","after":"<40 hex>","repository":{"full_name":"acme/shop"}}`.

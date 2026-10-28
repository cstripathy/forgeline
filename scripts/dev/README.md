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

It also plays GitHub's side of **Connect with a GitHub App**: it serves the approval page, hands back
an app id, a webhook secret (`demo-webhook-secret-from-fake-github-0123`) and a private key, lists one
installation (`acme`, repository `acme/shop`) and issues installation tokens; each status it logs
says whether it was posted with `installation-token` or `personal-token`. In the console set the
GitHub API address in the *Connect with a GitHub App* card to `http://127.0.0.1:9911`, click
*Create the GitHub App*, then *Continue to GitHub* and *Create GitHub App*. It checks the app token's
shape and expiry but not its signature (the Go tests do).

It accepts the token `ghp_demo_token_value_1234`, knows one repository (`acme/shop`) whose commit
holds a small `forgeline.yaml`, and appends every commit status it receives to the log. In the
console set the API address to `http://127.0.0.1:9911` (plain http is allowed only for loopback),
the token to that value and the repository to `acme/shop`; saving shows a webhook secret once. Then
send a signed `push` event: the header `X-Hub-Signature-256: sha256=<HMAC-SHA256 of the body with
the secret>`, `X-GitHub-Event: push`, and a body like
`{"ref":"refs/heads/main","after":"<40 hex>","repository":{"full_name":"acme/shop"}}`.

## `fake-sso.py`: a stand-in for AWS IAM Identity Center sign-in

```sh
python3 scripts/dev/fake-sso.py 9922
FORGELINE_DEV_SSO_BASE=http://127.0.0.1:9922 PATH=/tmp/fake-aws/bin:$PATH \
  ./bin/forgeline server --listen 127.0.0.1:8081 --data-dir /tmp/fl-sim
```

In the console open Infrastructure → AWS account → Connect, enter any `https://d-….awsapps.com/start`
address, and open the link the page shows (it is served by this script) to approve. Then pick one
of the two demo accounts. The server refuses `FORGELINE_DEV_SSO_BASE` unless it is a loopback
address. Role credentials are fake, so use it together with `fake-aws`.

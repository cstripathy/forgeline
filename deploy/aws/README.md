# Deploying the Forgeline demo on AWS

A small, private demo: one `t4g.small` EC2 instance in a default-VPC public subnet, running the
Forgeline server and a Postgres 18 container. It has **no inbound ports**. You reach the web UI
through SSM Session Manager port forwarding, so only people with AWS access to the account can
connect, and they still need the bearer token.

**This is not a hardened deployment.** There is no sandbox before milestone M3. Anyone holding the
token can run arbitrary containers on the instance, and the `forgeline` service user is in the
`docker` group, which is root-equivalent on that host. Keep it private and throwaway.

## What it creates

One CloudFormation stack (`forgeline-demo`) with: an IAM role and instance profile (SSM core plus
read access to one parameter), a security group with no inbound rules, and one encrypted 20 GB gp3
instance (IMDSv2 required, no key pair). The bearer token lives in an SSM SecureString parameter
that you create first; it is never in the template.

## Cost (ap-south-1, on-demand prices checked 2026-10-21)

| Item | Price | Per month (730 h) |
| --- | --- | --- |
| `t4g.small` | $0.0112/h | about $8.18 |
| 20 GB gp3 | $0.0912/GB-month | about $1.82 |
| Public IPv4 address | $0.005/h | about $3.65 |
| SSM, Parameter Store (standard), CloudFormation | free | $0 |
| **Total** | | **about $13.7** |

Data transfer and image pulls are small but not zero. Check your own bill; this is an estimate.

## Deploy

```sh
REGION=ap-south-1

# 1. A random token, stored encrypted. The value is not printed.
aws ssm put-parameter --region $REGION --name /forgeline-demo/token --type SecureString \
  --value "$(head -c 32 /dev/urandom | base64 | tr -d '/+=' )" \
  --tags Key=Project,Value=forgeline-demo

# 2. The stack. Pin GitRef to a full commit SHA you have reviewed.
aws cloudformation create-stack --region $REGION --stack-name forgeline-demo \
  --template-body file://deploy/aws/forgeline-demo.yaml --capabilities CAPABILITY_IAM \
  --parameters ParameterKey=GitRef,ParameterValue=<40-char-sha> \
               ParameterKey=VpcId,ParameterValue=<vpc-id> \
               ParameterKey=SubnetId,ParameterValue=<public-subnet-id>
aws cloudformation wait stack-create-complete --region $REGION --stack-name forgeline-demo
```

The stack finishes when the instance exists; the instance then needs a few more minutes to install
Docker, build Forgeline from source and start. Watch `/var/log/forgeline-bootstrap.log` through
Session Manager (`aws ssm start-session --target <instance-id>`).

## Use it

You need the AWS CLI and the [Session Manager plugin](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html) on your own machine.

```sh
# Token for the sign-in page (stack output TokenCommand prints this command):
aws ssm get-parameter --region ap-south-1 --name /forgeline-demo/token --with-decryption \
  --query Parameter.Value --output text

# Tunnel (stack output TunnelCommand); leave it running, then open http://127.0.0.1:8080/
aws ssm start-session --region ap-south-1 --target <instance-id> \
  --document-name AWS-StartPortForwardingSession --parameters portNumber=8080,localPortNumber=8080
```

To submit a pipeline from your laptop through the tunnel:

```sh
export FORGELINE_TOKEN=<token> FORGELINE_SERVER=http://127.0.0.1:8080
./bin/forgeline submit --wait examples/hello/pipeline.yaml
```

## Tear down

```sh
aws cloudformation delete-stack --region ap-south-1 --stack-name forgeline-demo
aws cloudformation wait stack-delete-complete --region ap-south-1 --stack-name forgeline-demo
aws ssm delete-parameter --region ap-south-1 --name /forgeline-demo/token
```

Deleting the stack removes the instance, its disk, the role, the instance profile and the security
group. The parameter is separate because CloudFormation cannot create SecureStrings, so delete it
yourself. Run history lives on the instance's disk and is deleted with it.

## Updating

Instances are replaced, not patched: create a new stack with a new `GitRef`, then delete the old one.

## Secrets for steps

A step can request a secret with `permissions: {secrets: [NAME]}`. The value is read from
`FORGELINE_SECRET_NAME` in the server's environment. On this deployment the service loads
`/etc/forgeline/env`, so add a line there as root (through Session Manager) and restart:

```sh
echo 'FORGELINE_SECRET_NPM_TOKEN=...' | sudo tee -a /etc/forgeline/env >/dev/null
sudo systemctl restart forgeline
```

The value stays on the instance's disk in that file (mode 0600 is worth checking), so this is
fine for a throwaway demo and not a secret store. Runs started by a GitHub webhook receive no
secrets yet. See `docs/design/002-spec-extensions.md` for the rules.

## Environment templates

`dev-box.yaml` (a private EC2 instance with an encrypted disk, IMDSv2 required, no inbound ports,
reachable through Session Manager) `qa-sandbox.yaml` (an on-demand DynamoDB table and an encrypted
SQS queue), `lambda-sandbox.yaml` (a small function behind a private, IAM-authenticated URL, with its
own role that can only write its own log group), `event-pipeline.yaml` (an encrypted SNS topic into an
encrypted SQS queue with a dead-letter queue) and `workflow-sandbox.yaml` (a Step Functions state
machine whose role has no permissions) are what the console's **Environments** page deploys. They are created for a person for a
limited time: the `Owner` and `ExpiresAt` parameters are recorded as tags, and Forgeline deletes the
stack when the time is up. Check them any time without creating anything:

```sh
aws cloudformation validate-template --template-body file://deploy/aws/dev-box.yaml
```

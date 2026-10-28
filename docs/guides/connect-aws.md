---
title: Connect AWS
parent: Guides
nav_order: 4
---

# Connect AWS

Go to **Infrastructure → AWS account → Connect**. Two tabs:

## Sign in with AWS (IAM Identity Center)

1. Enter your **access portal URL** (like `https://d-xxxxxxxxxx.awsapps.com/start`, from the IAM Identity Center page) and the region of your Identity Center.
2. Forgeline shows a short **code** and a link. Open the link in your own browser and approve.
3. The dialog continues by itself. Pick an **account** and a **role**, and the region Forgeline should work in.
4. **Test and connect.**

![Code]({{ '/console/23-aws-signin-code.png' | relative_url }})
![Choose]({{ '/console/24-aws-signin-choose.png' | relative_url }})

No AWS CLI is needed for the sign-in. Forgeline keeps the sign-in token sealed in its vault and
the role credentials only in memory, and passes them to the `aws` command as environment variables.

{: .important }
Pick a **read-only role** if you only want to look. Changes always need a second administrator's approval, but they run with whichever role you picked.

## Server profile
Uses a named profile of the AWS CLI on the machine that runs the server. Use it when the server
already has credentials (for example an instance role). Sign in on the server with `aws sso login`.

## Looking and changing

- **Stacks, instances, spend.** Spend loads only when you ask, because AWS bills each Cost Explorer request.
- **Cloud changes.** A developer or admin proposes a change (deploy a template, or start, stop or reboot an instance) with a reason. Forgeline previews it as a CloudFormation change set. A **different** administrator approves or rejects. Approval runs it and writes the audit log.

![Preview]({{ '/console/23-deploy-preview.png' | relative_url }})
![Done]({{ '/console/25-change-succeeded.png' | relative_url }})

**Verified:** reading a real account, the real device-code sign-in and the real credential step, and rejecting a change on a real account. Executing an approved change was run on a real account: a deploy and a delete of the demo stack, each approved by a second actor in the browser. Design: [note 009](https://github.com/forgelinehq/forgeline/blob/main/docs/design/009-aws-signin.md).

---
title: Connect GitHub
parent: Guides
nav_order: 3
---

# Connect GitHub

Pushes to chosen branches start runs of the `forgeline.yaml` at the repository root, and the
result is posted back as a commit status. Pull requests from forks never start runs.

Go to **Integrations → GitHub**. There are two ways in.

## A. GitHub App (recommended)

1. Click **Create the GitHub App**. Enter the owner (your account or an organization) and the address people reach this server at.
2. The console sends you to GitHub with the app's settings filled in. Approve once.
3. GitHub sends you back; Forgeline stores the app's key and webhook secret in its vault.
4. Click **Install**, pick the installation, then tick the repositories and branches that may start runs.

![Create the app]({{ '/console/26-github-app-create.png' | relative_url }})
![Choose repositories]({{ '/console/28-github-app-connected.png' | relative_url }})

Forgeline mints short-lived tokens by itself, so there is no personal token to rotate.

{: .note }
GitHub can only deliver webhooks to a public **https** address. On a laptop the webhook is created switched off; put the server behind TLS or a tunnel and switch it on.

**Verified:** the whole flow against a stand-in for GitHub, including a signed webhook starting a run and statuses posted with an installation token. The app was also created and installed on github.com, and the connection test read 19 repositories with an installation token. **Not yet:** a real webhook delivery or a commit status on a real repository. Design: [note 010](https://github.com/forgelinehq/forgeline/blob/main/docs/design/010-github-app.md).

## B. Token

Create a fine-grained token with *contents: read* and *commit statuses: write*, paste it with a webhook secret, add `owner/repo:branch` rules, and use **Test connection**. The token is write-only.

![Token form]({{ '/console/12-github-form.png' | relative_url }})
![Deliveries]({{ '/console/15-github-deliveries.png' | relative_url }})

The deliveries list shows every webhook as accepted, ignored or rejected, and why.

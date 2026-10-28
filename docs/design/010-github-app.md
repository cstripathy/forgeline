# 010: Connecting GitHub from the console with a GitHub App

Status: **Built**, verified against a stand-in only. Date: 2026-10-28. Pictures: [26](../console/26-github-app-create.png), [27](../console/27-github-app-install.png), [28](../console/28-github-app-connected.png).

## Question

Connecting GitHub meant creating a personal access token, pasting it, generating a webhook secret,
copying it into GitHub by hand and adding the webhook there. Can an administrator do it all from the
console, and end up with something safer than a long-lived personal token?

## Decision

A **GitHub App created from the console** (GitHub's manifest flow). The administrator enters this
server's public address and clicks one button. GitHub shows one approval page. GitHub then sends the
browser back with a one-time code, and Forgeline trades the code for the app's id, private key and
webhook secret. The administrator installs the app on an account or organisation, ticks repositories,
and saves. Nothing is typed or copied.

| Point | Choice |
| --- | --- |
| App permissions | Read for contents, metadata and pull requests; write for commit statuses; events `push` and `pull_request`; **private** to its owner. Nothing else |
| Credentials | The private key and webhook secret are sealed in the vault under internal names a pipeline cannot request, and never returned or audited. The client secret GitHub also sends is discarded |
| Token use | An RS256 app token (nine minutes) proves the app; it is traded for an installation token (about an hour), cached once per installation and renewed when under five minutes remain. The old personal-token path stays for people who need it, and `--github-allow` flags still work |
| The manifest form | The console's own pages forbid forms. The manifest must be sent to GitHub as a form post, so the server serves one small page for it: no script, a policy that lets a form go only to the one GitHub address, `no-store`, no referrer. It exists only for the person who started the setup |
| The callback | GitHub redirects the browser back from another site, so the session cookie (SameSite=Strict) is not sent. The callback is therefore bound to the setup by a one-time state value: it belongs to the administrator who started it, lives 15 minutes, is used once, and at most eight are open. A wrong state or code stores nothing |
| Choosing an installation | Chosen in the console from GitHub's own list of this app's installations, not from a redirect, so a crafted link cannot select someone else's. The id is checked with the app token before it is used, and each chosen repository must belong to the installation |
| Webhook reachability | GitHub can only deliver to a public https address. On a laptop the webhook is created switched off and the page says so; the rest works, and a tunnel or a real address turns it on |
| One app | A second app cannot be created while one exists; removing the integration deletes the key and secrets |

## What was checked

Go tests (race detector) against a stand-in for GitHub that verifies the app token's real RS256
signature, issuer and lifetime with the public key: the launch page's policy, its absence of script
and the manifest's permissions; an administrator can only continue their own setup; wrong state or
code, and reuse of a state, store nothing; secrets, key and client secret appear in no endpoint or
audit entry; installations come from GitHub; a foreign installation or an unreachable repository is
refused; a push signed with the app's own webhook secret starts a run fetched and reported with an
installation token; saving the form again keeps the app; a token near expiry is renewed; removal
deletes the key. Eight of those rules were removed one at a time and a test failed each time (the first
run found that two checks were weak: the token cache was not shared, which the test then exposed).

In a real browser against `scripts/dev/fake-github.py`: the card, the launch page, the approval page,
the return to the console, the installation and repository choice, then a signed webhook that started
a run whose statuses reached the stand-in with an installation token.

**Checked on real GitHub (2026-10-28):** the owner created an app from the console (manifest
flow), installed it, and chose 19 repositories. The console's connection test then read all 19
through an installation token, with no personal token stored. The manifest endpoints, the `pem`
format and the installation calls all worked as documented.

**Not checked:** a real webhook delivery (the address was a public site, not this laptop, so
nothing reached the server), a commit status posted to a real repository (a visible write, so it
was left for the owner), and GitHub Enterprise Server (an `/api/v3` address is accepted but untried).

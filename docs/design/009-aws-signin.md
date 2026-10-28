# 009: Signing in to AWS from the console

Status: **Built.** Date: 2026-10-28. Pictures: [23](../console/23-aws-signin-code.png), [24](../console/24-aws-signin-choose.png), [25](../console/25-aws-signin-connected.png).

## Question

Connecting an AWS account meant typing a profile name that already existed and was already logged
in on the server. People want to connect from the console itself, without a shell on the server and
without typing keys. What is the safest way?

## Options considered

| Option | Verdict |
| --- | --- |
| **IAM Identity Center (SSO) device sign-in** | Built. The person enters the access portal URL, is shown a code and a link, approves in their own browser, and picks an account and role. No password or key is typed into Forgeline; credentials are short-lived |
| Assume a role from the server's own identity | Already possible with the profile mode on a server with an IAM role. Not changed |
| Paste an access key and secret | Not built. The keys do not expire and would sit in the vault. The profile mode remains for people who must |

## Decisions

| Decision | Choice |
| --- | --- |
| Protocol | The documented IAM Identity Center OIDC device flow and the portal's account, role and credential calls, over HTTPS to `oidc.<region>.amazonaws.com` and `portal.sso.<region>.amazonaws.com`. The start address is only ever sent in a request body; the region must look like `ap-south-1`, so a person cannot aim the server at another host. A link AWS returns must be a web address |
| What is stored | The SSO access token (valid hours) is sealed in the vault under an internal name; the account and role in settings. Role credentials (about an hour) are kept in memory only and refreshed when under five minutes remain |
| How the `aws` command gets them | As environment variables for that one command, never on its command line. `AWS_PROFILE` is cleared so a profile on the server cannot override the chosen role. The connection's profile is the reserved name `forgeline-sso`, which a typed profile cannot use, so change requests keep working unchanged: each records the account and is refused if the account differs at execution |
| Who | Only administrators sign in, choose or disconnect. A pending sign-in belongs to the person who started it; another administrator polling it learns nothing and cannot end it. At most eight are pending, each for at most 15 minutes. AWS's requested polling pause is honoured |
| When it expires | The page says the sign-in expired and to sign in again, rather than using stale access |
| Audit | Start, sign-in, denial and sign-out are recorded; tokens and keys never are |

## What was checked

Go tests with the race detector, against a stand-in for the OIDC and portal endpoints: input
checks, the pause between polls, approval, account and role lists, that connecting runs a real
identity call, that `aws` receives role credentials in its environment and neither a profile flag
nor any credential in its arguments, that credentials are cached and refreshed before they expire,
that no endpoint or audit entry exposes a token or key, expiry, sign-out, denial, another
administrator's poll, and a `javascript:` link from the server. Six of those rules were removed one
at a time and a test failed each time (one survived at first and got its own test). Writing the
tests also found that another administrator's poll could delete someone's pending sign-in; fixed.

In a real browser against a stand-in (`scripts/dev/fake-sso.py` with `fake-aws`): the code and link,
automatic continuation after approval, choosing account and role, connecting, and the AWS page
reading through the sign-in.

**Not checked:** see the end of this note for the run against the real Identity Center.

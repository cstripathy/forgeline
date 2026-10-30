# 013 AI use cases: Claude in the console, talking to the infrastructure

Status: research note, 2026-10-30. Nothing here is built. It builds on note 007 (agent steps),
note 008 (the assistant) and note 005 (threat model), and on [012](012-market-research.md).

## How much to trust this

Same caveat as note 012: the sources are mostly vendor blogs and single case studies. Figures
such as "73% of failures are automatable" are unverified and are not used for decisions here.
Official Anthropic documentation was not fetched; the notes about Claude Code flags and the
GitHub Action come from third-party guides, so check the current docs before building on them.

## 1. Where we start

| Piece | Today |
| --- | --- |
| Assistant (008) | Answers in text, **no tools**, sees only what the page shows, everything gathered is wrapped as untrusted data. Cannot change anything |
| Agent steps (007) | A pipeline step runs Claude Code in a container, read-only tools by default, credential from the vault |
| Cloud changes (009) | Anyone proposes, a second administrator approves, then it runs |
| Missing | Source checkout for console runs, a way for the assistant to look things up itself, and any way to turn an answer into an action |

This is a good base. The risky part of AI features is not the model, it is what the model is
allowed to do. We already separate "answer" from "act", and every use case below keeps that.

## 2. Use cases people actually report

All cited from vendor or practitioner write-ups ([GeekyAnts](https://geekyants.com/blog/how-we-built-an-ai-agent-that-fixes-cicd-pipeline-failures-automatically), [Halodoc via daily.dev](https://daily.dev/posts/automating-root-cause-analysis-for-ci-cd-failures-using-ai-at-halodoc-tspinvhis), [Agentic Blog](https://blog.appxlab.io/2026/03/31/self-healing-cicd-pipeline-ai-agents/), [DEV](https://dev.to/actocodes/agentic-cicd-how-ai-agents-are-building-self-healing-pipelines-that-auto-fix-broken-builds-2m91)).

1. **Explain a failed run.** Read the log, say what broke and what to do. We have this in the assistant.
2. **Tiered triage.** Halodoc's design: deterministic rules first, a cache of known failure signatures second, the model only for unfamiliar failures. They report about 60% of failures resolved without calling a model and about 65% full agreement with engineers on the model tier (self-reported, small sample). Lesson: use the model last, it is the slow and costly tier.
3. **Draft a fix as a pull request.** Classify, propose a patch, run it through the same pipeline, open a PR for a human. Advice repeated across sources: begin with a read-only diagnosis posted to the PR or chat, and only auto-fix narrow safe classes (formatting, lint, a known dependency conflict, a flaky retry).
4. **Write and change pipelines.** Generate a pipeline from a repo, explain one, migrate from a GitHub Actions or Jenkins file, tighten permissions ("which of these steps could run with no network?").
5. **Review changes.** Summarise a pull request, flag risky changes to the pipeline file or deploy config.
6. **Release notes and change impact.** Summarise what a release contains ([paper](https://arxiv.org/pdf/2603.14619)).
7. **Ask about the infrastructure.** "What is running in the demo account?", "why did the last deploy change the security group?", "what would this change cost?".
8. **Propose infrastructure changes.** "Stop the idle instance", "add a rule", written as a proposal that a person approves.

## 3. How Claude can talk to our infrastructure

Two ways exist. Choose by who is asking and what they may touch.

**A. Inside the console (the assistant gets read tools).** Give the assistant a small set of
functions the *server* runs for it, each limited to what the signed-in person could already see:
`get_run`, `get_step_log` (masked), `list_pipelines`, `get_pipeline`, `aws_summary`,
`list_changes`, `search_audit`. The model asks, the server checks the person's role, runs it, and
returns text wrapped as untrusted data. Nothing here can write.

**B. Outside the console (Forgeline as an MCP server).** Claude Code, an IDE or another agent
connects to Forgeline over the [Model Context Protocol](https://modelcontextprotocol.io) with a
scoped API token. Same read tools as A, plus `propose_*` tools that only *create a proposal*
(a pipeline version, a cloud change). The existing approval rule still applies: a second
administrator approves in the console. This answers "claude code on the board configures our
infra" without giving Claude the keys.

Neither route gives the model cloud credentials. It never calls AWS itself; Forgeline does, under
its own rules, and records who asked.

## 4. Risks and the rules that follow

Cited: [MCP security notes](https://dzone.com/articles/secure-mcp-servers), [Xygeni](https://xygeni.io/?p=67367), [OWASP "excessive agency" summaries](https://docs.bytehide.com/products/radar/sast/owasp/llm06-2025) (secondary sources; check OWASP directly). One often-repeated figure, that 43% of tested MCP servers had command injection flaws, is unverified.

| Risk | Rule for Forgeline |
| --- | --- |
| Prompt injection through logs, commit messages, PR text, tickets | Everything the model reads stays wrapped as untrusted data (already so). Tool results get the same wrapper. A model reading a log must not be able to trigger a write because of text in the log: **tools that write only create proposals** |
| Excessive agency / "least agency" | Per-tool allow list per backend and per role. No tool that deletes, applies or approves. Turn budgets and a cap on tool calls per question |
| Confused deputy: the agent uses a more powerful identity than the asker | Every tool call runs as the person who asked, with their role. MCP tokens are per person, scoped, expiring, revocable, and listed in the audit log |
| Broad credentials held by the tool server | The MCP server holds none of its own. It calls the same internal functions the console uses |
| Tool poisoning, shadow MCP servers | Forgeline exposes a fixed tool list and does not load third-party MCP servers into its own assistant. Agent steps that use other MCP servers need an explicit allow list in the pipeline |
| Approving without reading | A proposal shows the exact change and the model's stated reason side by side, and records that an AI wrote it. The proposer can never be the approver (already so for cloud changes) |
| Secrets in prompts | Secret values never enter any prompt (already so). Tool outputs pass the same masking as logs |
| Cost runaway | Per-person rate limits (exist), per-run token budget for agent steps, an org-wide monthly cap, and the usage shown in the console |

## 5. Proposed work, in order

| # | Item | Notes | Size |
| --- | --- | --- | --- |
| A1 | **Failure summary on the run page** | One call when a run fails, cached, shown above the graph. Deterministic rules first (exit code, known messages), model second. Gives users value without any new permission | S |
| A2 | **Assistant read tools** (section 3A) | Fixed tool list, role-checked, results wrapped as untrusted, audit entry per call, tool-call cap | M |
| A3 | **"Fix this" proposes a pipeline version** | The assistant drafts a change and saves it as an unapproved draft with a diff; a person reviews in the editor. No auto-apply | S |
| A4 | **Repo checkout for console runs** | Prerequisite for agent steps that read or change code (also item 1 in note 012) | M |
| A5 | **Agent step opens a pull request** | Runs in a container with a write-scoped GitHub token limited to a new branch; never pushes to a protected branch; human merges | M |
| A6 | **Forgeline MCP server** (section 3B) | Read tools first; `propose_pipeline_change` and `propose_cloud_change` second. Per-person scoped tokens | M |
| A7 | **Natural-language cloud change as a proposal** | "Stop the idle instance" becomes a proposal with the exact CloudFormation or API call shown; second administrator approves | M |
| A8 | **Importer with explanation** | Convert a GitHub Actions workflow to Forgeline YAML and explain every difference | M |
| A9 | **Flaky-test advisor** | Uses run history to name suspected flaky steps and suggest quarantine (needs note 012 item 12) | S |
| A10 | **Usage and budget page** | Tokens and cost per person, pipeline and feature; monthly cap | S |

Recommended first three: A1 (visible, safe), A2 (turns the assistant from a reader into a helper
that can look things up) and A4 (unblocks everything that touches code).

## 6. Decisions that need the maintainer

1. Should the assistant ever *apply* anything itself, even with a button, or always stay at "create a proposal"? Recommendation: proposals only, for the first release.
2. Which backend is the default for a team: the user's own Anthropic API key, or a Claude Code token? Recommendation: API key, because a shared login token means every asker uses one person's account.
3. May A5 hold a GitHub token that can write branches? Needs a GitHub App permission change and design note 011-style credential rules before building.

## 7. What this research could not tell us

- No independent evaluation of agent fix quality in CI. Start narrow and measure our own acceptance rate before widening.
- Nothing specific on AWS, Kubernetes or Terraform MCP safety. Our approach (propose, then human approval, no credentials to the model) is conservative by design, not copied from a source.
- Current official Claude Code and Agent SDK details were not read. Verify flags, permission modes and the GitHub Action inputs in the official docs before A5 and A6.

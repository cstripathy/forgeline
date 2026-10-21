# Forgeline: instructions for Claude Code

Forgeline is an open-source, self-hostable delivery platform where AI agents run safely.
Read `README.md` for the vision, `docs/ROADMAP.md` for milestones, and **`docs/PLAN.md` for
what to do next**. The project is pre-alpha and maintained by one person.

## Commands

- `make ci` runs vet, tests, and build. Run it before every push; CI runs the same thing.
- `make build` builds `./bin/forgeline`.
- Regenerate the JSON Schema after changing types in `internal/pipeline`:
  `go test ./internal/pipeline -run TestSchemaMatchesCommittedFile -update`

## Layout

- `cmd/forgeline`: command-line entry point. Keep `run` testable (it takes args and writers).
- `internal/pipeline`: the pipeline model, validation, and schema generation.
- `schema/`: generated JSON Schema. Never edit by hand.
- `docs/design/`: numbered design notes. Spec, trust-model, and interface changes start here.

## Principles that shape the code

1. The Go types are the source of truth. Schema and the YAML form derive from them.
2. Agents and untrusted input are dangerous by default. A step must never combine untrusted
   input, secrets, and external actions unless each is declared and granted.
3. No in-process plugins. Steps run as containers.
4. Local first: anything that runs in a cluster must also run on a laptop.
5. Humans approve anything that changes production. AI actions are audited.

## Conventions

- Go 1.24. Run `gofmt` and `go vet`. Table-driven tests. Error messages say what is wrong and
  where (for example `steps[2].needs[0]: unknown step "x"`).
- Small commits with a message that explains why. Keep `docs/ROADMAP.md` statuses accurate.
- Dependencies: keep them few. Each new one needs an Apache-2.0-compatible license (MIT, BSD,
  Apache-2.0, MPL-2.0 are fine; no GPL or AGPL) and a one-line reason in the commit message.
- GitHub Actions: pin every third-party action to a full commit SHA, never a tag. Keep
  `permissions: contents: read` by default. Verify a SHA against the action's repository
  before using it.
- Never commit secrets, tokens, or personal data. Use the GitHub noreply email for commits.
- Do not claim a feature works until a test or a run shows it. Say what is unverified.

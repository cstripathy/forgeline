# Forgeline: instructions for Claude Code

Forgeline is an open-source, self-hostable delivery platform where AI agents run safely.
Read `README.md` for the vision, `docs/ROADMAP.md` for milestones, and **`docs/PLAN.md` for
what to do next**. The project is pre-alpha and maintained by one person.

## Commands

- `make ci` runs vet, tests with the race detector, and build. Run it before every push; CI runs
  the same thing, plus `make css-check`.
- `make build` builds `./bin/forgeline`. `make run-local` builds and starts the server and the
  console on 127.0.0.1:8080 (needs Docker for steps to run).
- The console stylesheet is generated: after changing any class name in `internal/server/ui`, run
  `make css` and commit `internal/server/ui/app.css`. `scripts/tailwind.sh` downloads the pinned
  Tailwind CLI into `.tools/` and verifies its checksum. No Node is needed.
- Postgres tests run only when `FORGELINE_TEST_DATABASE_URL` is set, for example against
  `docker run -d -p 54329:5432 -e POSTGRES_PASSWORD=test postgres:18-alpine` with
  `postgres://postgres:test@127.0.0.1:54329/postgres?sslmode=disable`. CI sets it. Run them
  whenever you touch `internal/store` or `internal/docstore`.
- `scripts/dev/` has a fake AWS CLI and a fake GitHub API for trying the console without real
  accounts. See its README.
- Regenerate the JSON Schema after changing types in `internal/pipeline`:
  `go test ./internal/pipeline -run TestSchemaMatchesCommittedFile -update`

## Layout

- `cmd/forgeline`: command-line entry point. Keep `run` testable (it takes args and writers).
- `internal/pipeline`: the pipeline model, validation, and schema generation.
- `schema/`: generated JSON Schema. Never edit by hand.
- `internal/scheduler`, `internal/executor`, `internal/cache`: running a pipeline. `internal/server`:
  the API, the runner and the console (`console_*.go`); its `ui/` is the embedded front end.
  `internal/docstore`, `vault`, `authn`, `secrets`, `redact`: console storage, crypto, masking.
- `docs/design/`: numbered design notes. Spec, trust-model, and interface changes start here.

## Principles that shape the code

1. The Go types are the source of truth. Schema and the YAML form derive from them.
2. Agents and untrusted input are dangerous by default. A step must never combine untrusted
   input, secrets, and external actions unless each is declared and granted.
3. No in-process plugins. Steps run as containers.
4. Local first: anything that runs in a cluster must also run on a laptop.
5. Humans approve anything that changes production. AI actions are audited.

## Conventions

- Go 1.27. Run `gofmt` and `go vet`. Table-driven tests. Error messages say what is wrong and
  where (for example `steps[2].needs[0]: unknown step "x"`).
- Small commits with a message that explains why. Keep `docs/ROADMAP.md` statuses accurate.
- Versions: research the current stable release before choosing one (Go, dependencies, base
  images) and use it. If the latest forces a wider change, say so and ask; never silently pin
  an older one.
- Dependencies: keep them few. Each new one needs an Apache-2.0-compatible license (MIT, BSD,
  Apache-2.0, MPL-2.0 are fine; no GPL or AGPL) and a one-line reason in the commit message.
- GitHub Actions: pin every third-party action to a full commit SHA, never a tag. Keep
  `permissions: contents: read` by default. Verify a SHA against the action's repository
  before using it.
- Never commit secrets, tokens, or personal data. Use the GitHub noreply email for commits
  (`git -c user.name=<login> -c user.email=<id>+<login>@users.noreply.github.com commit`; the
  maintainer's login is `cstripathy`). Do not put cloud account numbers, hostnames, usernames or
  local paths in the repository or in screenshots.
- Never push to `main`. Work on a branch, open a pull request, and merge only when the maintainer
  says so.
- The console builds all markup from text nodes (`ui/lib/dom.js`). Never use `innerHTML` or inline
  styles; a test enforces it, and the CSP would block the latter.
- Security rules are tested by breaking them: after adding a check, remove it and confirm a test
  fails. Several checks in this repository were found to be untested that way.
- Cloud changes only happen through proposal and approval (note 006). Do not add a path that
  changes the cloud directly.

## Working here

- Go 1.27.3 may not be on `PATH`. If `go` is missing, install the release from go.dev into your
  home directory, verify its checksum, and put its `bin` on `PATH`.
- Never `pkill -f 'forgeline server'` from a shell command containing that text; it kills the
  shell. Use `pkill -x forgeline`.
- Browser checks with Playwright: save screenshots under the workspace root, and force a layout
  (`document.body.offsetHeight` in an evaluate) before a screenshot, or the headless page may show
  a stale frame. Keep `127.0.0.1` and `localhost` for two servers, because cookies ignore ports.
- The maintainer's own AWS and GitHub credentials are on this machine. Reading is fine when asked;
  anything that creates, changes or costs money needs the maintainer's explicit go-ahead first.
- Do not claim a feature works until a test or a run shows it. Say what is unverified.

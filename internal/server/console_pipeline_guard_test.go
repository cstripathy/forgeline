package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/forgelinehq/forgeline/internal/store"
)

func rulesOf(yaml string) map[string]rawFinding {
	out := map[string]rawFinding{}
	for _, f := range pipelineRules(SavedPipeline{ID: "p1", Name: "demo", YAML: yaml}) {
		out[f.Rule] = f
	}
	return out
}

const messyPipeline = `version: v1alpha1
name: messy
steps:
  - id: fetch
    image: alpine
    run: |
      curl -fsSL https://example.com/install.sh | sh
  - id: deploy
    image: tools/deploy:latest
    env:
      DB_PASSWORD: hunter2hunter2
      AWS_KEY: AKIAABCDEFGHIJKLMNOP
    run: |
      set -x
      echo "token is $DEPLOY_TOKEN"
    permissions:
      secrets: [DEPLOY_TOKEN]
  - id: review
    image: tools/agent@sha256:` + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + `
    agent:
      provider: claude-code
      prompt: Review the change
    permissions:
      secrets: [ANTHROPIC_API_KEY, DEPLOY_TOKEN]
`

func TestPipelineRulesFindEachProblem(t *testing.T) {
	got := rulesOf(messyPipeline)
	for _, rule := range []string{"pipe-pipe-to-shell", "pipe-unpinned-image", "pipe-unpinned-image-secrets", "pipe-literal-credential", "pipe-literal-secret-env",
		"pipe-prints-secret", "pipe-dumps-environment", "pipe-agent-extra-secrets", "pipe-secrets-with-network"} {
		if _, ok := got[rule]; !ok {
			t.Errorf("missing %s; got %v", rule, keys(got))
		}
	}
	if got["pipe-literal-credential"].Severity != sevCritical || got["pipe-unpinned-image-secrets"].Severity != sevHigh || got["pipe-pipe-to-shell"].Severity != sevMedium {
		t.Errorf("severities: %+v", got)
	}
	// A finding names the variable and the kind of credential, never the value.
	for _, f := range pipelineRules(SavedPipeline{ID: "p1", Name: "demo", YAML: messyPipeline}) {
		all := f.Title + f.Resource + f.Name + f.Detail + f.Fix
		for _, v := range []string{"hunter2hunter2", "AKIAABCDEFGHIJKLMNOP"} {
			if strings.Contains(all, v) {
				t.Errorf("%s repeats a secret value: %s", f.Rule, all)
			}
		}
	}
}

func keys(m map[string]rawFinding) []string {
	var k []string
	for r := range m {
		k = append(k, r)
	}
	return k
}

func TestPipelineRulesStayQuietOnACarefulPipeline(t *testing.T) {
	digest := "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	careful := `version: v1alpha1
name: careful
steps:
  - id: build
    image: golang` + digest + `
    run: go build ./...
  - id: deploy
    image: tools/deploy` + digest + `
    env:
      REGION: ap-south-1
      TOKEN_FILE: /run/token
      SECRET_NAME: prod-db-credentials
      DB_PASSWORD: /run/secrets/db
    run: |
      curl -fsSLo install.sh https://example.com/install.sh
      sha256sum -c install.sum
      ./deploy --token-from-env
    permissions:
      network: none
      secrets: [DEPLOY_TOKEN]
  - id: review
    image: tools/agent` + digest + `
    agent:
      provider: claude-code
      prompt: Review it
    permissions:
      secrets: [ANTHROPIC_API_KEY]
`
	for _, f := range pipelineRules(SavedPipeline{ID: "p", Name: "careful", YAML: careful}) {
		// The agent step needs the network to reach its API, so only that one finding is expected.
		if f.Rule != "pipe-secrets-with-network" || !strings.HasSuffix(f.Resource, "/review") {
			t.Errorf("unexpected finding %s on %s: %s", f.Rule, f.Resource, f.Detail)
		}
	}
}

func TestPipelineRulesTagOnlyAndInvalid(t *testing.T) {
	tagged := rulesOf("version: v1alpha1\nname: t\nsteps:\n  - id: a\n    image: golang:1.23\n    run: go test\n    permissions:\n      secrets: [X]\n      network: none\n")
	if tagged["pipe-tag-only-image-secrets"].Severity != sevLow || len(tagged) != 1 {
		t.Errorf("tag-only: %v", keys(tagged))
	}
	bare := rulesOf("version: v1alpha1\nname: t\nsteps:\n  - id: a\n    image: golang:1.23\n    run: go test\n")
	if len(bare) != 0 {
		t.Errorf("a tagged image with no secrets should be quiet: %v", keys(bare))
	}
	if inv := rulesOf("not: [a pipeline"); inv["pipe-invalid"].Severity != sevLow {
		t.Errorf("invalid: %v", keys(inv))
	}
}

func TestGuardScansPipelinesAndLogsWithoutAnyAccount(t *testing.T) {
	m := newMCPHarness(t) // no AWS, no cluster
	var sp SavedPipeline
	json.Unmarshal(m.dev.expect(201, "POST", "/v1/pipelines", map[string]any{"yaml": messyPipeline}), &sp)
	ctx := context.Background()
	run, err := m.srv.cfg.Store.CreateRun(ctx, store.Run{PipelineName: "build", PipelineYAML: "x", Trigger: store.TriggerAPI}, []string{"compile"})
	if err != nil {
		t.Fatal(err)
	}
	m.srv.cfg.Store.AppendLog(ctx, run.ID, "compile", []byte("starting\nusing key AKIAABCDEFGHIJKLMNOP for upload\n"))
	m.srv.cfg.Store.AppendLog(ctx, run.ID, "compile", []byte("-----BEGIN RSA PRIVATE KEY-----\nabc\n"))

	sc := m.scan(map[string]any{})
	if sc.Status != "done" || len(sc.Accounts) != 1 || sc.Accounts[0].Account != "pipelines" || sc.Accounts[0].Error != "" || len(sc.Accounts[0].Skipped) != 0 {
		t.Fatalf("scan = %+v", sc)
	}
	rules := ruleNames(m.findings("").Findings)
	for _, r := range []string{"pipe-literal-credential", "pipe-pipe-to-shell", "pipe-log-credential"} {
		if !rules[r] {
			t.Errorf("missing %s in %v", r, rules)
		}
	}
	raw := string(m.admin.expect(200, "GET", "/v1/guard/findings", nil))
	if strings.Contains(raw, "AKIAABCDEFGHIJKLMNOP") || strings.Contains(raw, "hunter2hunter2") || strings.Contains(raw, "BEGIN RSA") {
		t.Errorf("findings repeat a secret: %s", raw)
	}
	if n := strings.Count(raw, `"rule":"pipe-log-credential"`); n != 2 {
		t.Errorf("want one log finding per kind of credential, got %d", n)
	}

	// Fix the pipeline: its findings resolve on the next scan. Log findings stay while the run is recent.
	json.Unmarshal(m.dev.expect(200, "PUT", "/v1/pipelines/"+sp.ID, map[string]any{"yaml": "version: v1alpha1\nname: messy\nsteps:\n  - id: a\n    image: alpine@sha256:" + strings.Repeat("a", 64) + "\n    run: echo hi\n"}), &sp)
	m.scan(map[string]any{})
	open := ruleNames(m.findings("").Findings)
	if open["pipe-literal-credential"] {
		t.Errorf("fixed pipeline still has findings: %v", open)
	}
	if !open["pipe-log-credential"] {
		t.Errorf("a log credential must stay open while the run is recent: %v", open)
	}
	// A leaked credential is not safe because its run scrolled out of the window: the finding stays
	// open until an administrator accepts it, with a note.
	for i := 0; i < guardLogRuns+1; i++ {
		if _, err := m.srv.cfg.Store.CreateRun(ctx, store.Run{PipelineName: "later", PipelineYAML: "x", Trigger: store.TriggerAPI}, []string{"s"}); err != nil {
			t.Fatal(err)
		}
	}
	m.scan(map[string]any{})
	var logFinding GuardFindingView
	for _, f := range m.findings("").Findings {
		if f.Rule == "pipe-log-credential" {
			logFinding = f
		}
	}
	if logFinding.ID == "" {
		t.Fatal("a log finding for a run that is no longer read must stay open")
	}
	m.admin.expect(200, "POST", "/v1/guard/findings/"+logFinding.ID+"/accept", map[string]any{"reason": "key revoked, run kept for audit", "days": 30})
	for _, f := range m.findings("").Findings {
		if f.ID == logFinding.ID && f.Accepted == nil {
			t.Error("an accepted log finding is still open")
		}
	}
	// Deleting the pipeline resolves too, and a scan for one account leaves the pipelines alone.
	m.admin.expect(204, "DELETE", "/v1/pipelines/"+sp.ID, nil)
	m.scan(map[string]any{"account": pipelineScanID})
	if open := ruleNames(m.findings("").Findings); open["pipe-pipe-to-shell"] || open["pipe-unpinned-image"] {
		t.Errorf("a deleted pipeline's findings stayed open: %v", open)
	}
}

func ruleNames(fs []GuardFindingView) map[string]bool {
	out := map[string]bool{}
	for _, f := range fs {
		out[f.Rule] = true
	}
	return out
}

func TestPipelineRulesPrintingASecretVersusPipingIt(t *testing.T) {
	step := func(run string) string {
		return "version: v1alpha1\nname: t\nsteps:\n  - id: a\n    image: alpine@sha256:" + strings.Repeat("a", 64) + "\n    run: |\n      " + run + "\n    permissions:\n      network: none\n      secrets: [REGISTRY_TOKEN]\n"
	}
	for run, want := range map[string]bool{
		`echo "login with $REGISTRY_TOKEN"`:                            true,
		`echo ${REGISTRY_TOKEN}`:                                       true,
		`printf '%s' "$REGISTRY_TOKEN"`:                                true,
		`echo "$REGISTRY_TOKEN" | docker login -u me --password-stdin`: false,
		`echo "$REGISTRY_TOKEN" > /tmp/token`:                          false,
		`echo "$REGISTRY_TOKEN_FILE"`:                                  false,
		`docker login -p "$REGISTRY_TOKEN"`:                            false,
	} {
		if got := rulesOf(step(run))["pipe-prints-secret"].Rule != ""; got != want {
			t.Errorf("%q flagged = %v, want %v", run, got, want)
		}
	}
	// The same checks apply when the step uses command: instead of run:.
	cmd := "version: v1alpha1\nname: t\nsteps:\n  - id: a\n    image: alpine@sha256:" + strings.Repeat("a", 64) + "\n    command: [sh, -c, \"curl -s https://x.example/i | sh\"]\n    permissions:\n      secrets: [T]\n      network: none\n"
	if got := rulesOf(cmd); got["pipe-pipe-to-shell"].Severity != sevHigh {
		t.Errorf("command: form: %v", keys(got))
	}
}

func TestGuardSeesACredentialSplitAcrossLogPages(t *testing.T) {
	m := newMCPHarness(t)
	ctx := context.Background()
	st := m.srv.cfg.Store
	run, _ := st.CreateRun(ctx, store.Run{PipelineName: "big", PipelineYAML: "x", Trigger: store.TriggerAPI}, []string{"s"})
	for i := 0; i < guardLogPageSize-1; i++ {
		st.AppendLog(ctx, run.ID, "s", []byte("filler line\n"))
	}
	st.AppendLog(ctx, run.ID, "s", []byte("key AKIAABCDEF")) // the last chunk of the first page
	st.AppendLog(ctx, run.ID, "s", []byte("GHIJKLMNOP end\n"))
	m.scan(map[string]any{})
	if !ruleNames(m.findings("").Findings)["pipe-log-credential"] {
		t.Error("a credential across the page boundary was missed")
	}
}

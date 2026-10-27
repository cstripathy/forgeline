package server

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/cstripathy/forgeline/internal/authn"
	"github.com/cstripathy/forgeline/internal/docstore"
	"github.com/cstripathy/forgeline/internal/executor"
)

const withSecret = `version: v1alpha1
name: deploy
steps:
  - id: build
    image: alpine:3.24
    run: make
  - id: ship
    image: alpine:3.24
    run: ./ship
    needs: [build]
    permissions:
      secrets: [DEPLOY_KEY]
`

func TestValidateReturnsGraphProblemsAndWarnings(t *testing.T) {
	h := newConsoleHarness(t)
	admin := h.adminBrowser("ada")

	var v ValidateJSON
	json.Unmarshal(admin.expect(200, "POST", "/v1/pipelines/validate", map[string]string{"yaml": withSecret}), &v)
	if !v.Valid || v.Name != "deploy" || v.Graph == nil || len(v.Graph.Steps) != 2 {
		t.Fatalf("validate = %+v", v)
	}
	if len(v.Graph.Layers) != 2 || v.Graph.Steps[1].Layer != 1 || v.Graph.Steps[1].Needs[0] != "build" {
		t.Errorf("graph = %+v", v.Graph)
	}
	if len(v.MissingSecrets) != 1 || v.MissingSecrets[0] != "DEPLOY_KEY" {
		t.Errorf("missing secrets = %v, want [DEPLOY_KEY]", v.MissingSecrets)
	}
	joined := strings.Join(v.Warnings, "\n")
	if !strings.Contains(joined, `"ship" holds secrets and has network access`) {
		t.Errorf("warnings = %v, want the secrets+network warning", v.Warnings)
	}

	json.Unmarshal(admin.expect(200, "POST", "/v1/pipelines/validate", map[string]string{"yaml": "version: v1alpha1\nname: x\nsteps:\n  - id: a\n    image: alpine\n    run: x\n    needs: [nope]\n"}), &v)
	if v.Valid || len(v.Problems) == 0 || !strings.Contains(v.Problems[0].Path, "needs") {
		t.Errorf("invalid pipeline: %+v", v)
	}
	json.Unmarshal(admin.expect(200, "POST", "/v1/pipelines/validate", map[string]string{"yaml": "name: [unclosed"}), &v)
	if v.Valid || len(v.Problems) == 0 {
		t.Errorf("syntax error not reported: %+v", v)
	}
	json.Unmarshal(admin.expect(200, "POST", "/v1/pipelines/validate", map[string]string{"yaml": "version: v1alpha1\nname: a\nsteps:\n  - id: a\n    image: alpine\n    run: x\n"}), &v)
	if !strings.Contains(strings.Join(v.Warnings, " "), "mutable image tag") {
		t.Errorf("an untagged image should warn: %v", v.Warnings)
	}
}

func TestPipelineLifecycleVersionsAndConflicts(t *testing.T) {
	h := newConsoleHarness(t)
	admin := h.adminBrowser("ada")
	admin.createUser("dev", authn.Developer)
	admin.createUser("view", authn.Viewer)
	dev, view := h.login("dev"), h.login("view")

	view.expect(403, "POST", "/v1/pipelines", map[string]any{"yaml": twoSteps})
	dev.expect(400, "POST", "/v1/pipelines", map[string]any{"yaml": "version: v1alpha1\nname: BAD NAME\nsteps: []\n"})
	var sp SavedPipeline
	json.Unmarshal(dev.expect(201, "POST", "/v1/pipelines", map[string]any{"yaml": twoSteps, "description": "demo build"}), &sp)
	if sp.ID != "demo" || sp.Version != 1 || sp.CreatedBy != "dev" {
		t.Fatalf("created = %+v", sp)
	}
	dev.expect(409, "POST", "/v1/pipelines", map[string]any{"yaml": twoSteps})

	v2 := strings.Replace(twoSteps, "make test", "make test -v", 1)
	json.Unmarshal(dev.expect(200, "PUT", "/v1/pipelines/demo", map[string]any{"yaml": v2, "base_version": 1, "description": "demo build"}), &sp)
	if sp.Version != 2 || len(sp.History) != 1 || sp.History[0].Version != 1 {
		t.Fatalf("after update = version %d, history %+v", sp.Version, sp.History)
	}
	// An editor that started from version 1 must be told the pipeline moved on.
	resp, body := dev.do("PUT", "/v1/pipelines/demo", map[string]any{"yaml": twoSteps + "# stale\n", "base_version": 1})
	if resp.StatusCode != 409 || !strings.Contains(string(body), "changed") {
		t.Fatalf("stale save = %d %s, want 409", resp.StatusCode, body)
	}
	// The name inside the YAML must match; renaming is a new pipeline.
	dev.expect(400, "PUT", "/v1/pipelines/demo", map[string]any{"yaml": strings.Replace(twoSteps, "name: demo", "name: other", 1)})
	// Saving unchanged text is a no-op, not a new version.
	json.Unmarshal(dev.expect(200, "PUT", "/v1/pipelines/demo", map[string]any{"yaml": v2, "base_version": 2, "description": "demo build"}), &sp)
	if sp.Version != 2 {
		t.Errorf("an unchanged save made version %d", sp.Version)
	}

	json.Unmarshal(dev.expect(200, "POST", "/v1/pipelines/demo/versions/1/restore", nil), &sp)
	if sp.Version != 3 || sp.YAML != twoSteps {
		t.Fatalf("restore: version %d, yaml restored=%v", sp.Version, sp.YAML == twoSteps)
	}
	dev.expect(404, "POST", "/v1/pipelines/demo/versions/99/restore", nil)

	var list struct{ Pipelines []PipelineListItem }
	json.Unmarshal(view.expect(200, "GET", "/v1/pipelines", nil), &list)
	if len(list.Pipelines) != 1 || list.Pipelines[0].Steps != 2 || list.Pipelines[0].Version != 3 {
		t.Errorf("list = %+v", list)
	}
	view.expect(200, "GET", "/v1/pipelines/demo", nil)
	view.expect(404, "GET", "/v1/pipelines/nope", nil)
	dev.expect(403, "DELETE", "/v1/pipelines/demo", nil) // deleting is an admin action
	admin.expect(204, "DELETE", "/v1/pipelines/demo", nil)
	admin.expect(404, "GET", "/v1/pipelines/demo", nil)
}

func TestSecretsAreWriteOnlyEncryptedAndMaskedInRuns(t *testing.T) {
	const value = "super-secret-deploy-key-123"
	var mu sync.Mutex
	var got map[string]string
	fake := &executor.Fake{Handler: func(_ context.Context, s executor.StepRun, logs io.Writer) (executor.Result, error) {
		if s.ID == "ship" {
			mu.Lock()
			got = s.Secrets
			mu.Unlock()
			io.WriteString(logs, "shipping with "+s.Secrets["DEPLOY_KEY"]+"\n")
		}
		return executor.Result{}, nil
	}}
	h := newHarness(t, fake)
	admin := h.adminBrowser("ada")
	admin.createUser("dev", authn.Developer)
	dev := h.login("dev")

	// Only an admin writes secrets; a developer may see that they exist.
	dev.expect(403, "PUT", "/v1/secrets/DEPLOY_KEY", map[string]string{"value": value})
	admin.expect(400, "PUT", "/v1/secrets/1BAD", map[string]string{"value": value})
	admin.expect(400, "PUT", "/v1/secrets/DEPLOY_KEY", map[string]string{"value": "short"})
	admin.expect(400, "PUT", "/v1/secrets/DEPLOY_KEY", map[string]string{"value": "line1-line1\nline2-line2"})
	out := admin.expect(201, "PUT", "/v1/secrets/DEPLOY_KEY", map[string]string{"value": value, "description": "prod deploy key"})
	if strings.Contains(string(out), value) {
		t.Fatalf("the create response echoes the value: %s", out)
	}

	// Nothing the API returns contains the value.
	for _, b := range []*browser{admin, dev} {
		for _, path := range []string{"/v1/secrets", "/v1/audit", "/v1/overview", "/v1/pipelines"} {
			resp, body := b.do("GET", path, nil)
			if resp.StatusCode == 200 && strings.Contains(string(body), value) {
				t.Errorf("GET %s leaks the secret", path)
			}
		}
	}
	// At rest it is sealed.
	docs, _ := h.srv.docs.List(context.Background(), kindSecret, docstore.ListOptions{})
	if len(docs) != 1 || strings.Contains(string(docs[0].Body), value) {
		t.Fatalf("stored secret document = %s", docs[0].Body)
	}

	var list struct{ Secrets []SecretJSON }
	json.Unmarshal(dev.expect(200, "GET", "/v1/secrets", nil), &list)
	if len(list.Secrets) != 1 || list.Secrets[0].Name != "DEPLOY_KEY" || list.Secrets[0].Source != "vault" || list.Secrets[0].Version != 1 {
		t.Fatalf("list = %+v", list)
	}

	// A saved pipeline can use it, and its output is masked.
	dev.expect(201, "POST", "/v1/pipelines", map[string]any{"yaml": withSecret})
	json.Unmarshal(dev.expect(200, "GET", "/v1/secrets", nil), &list)
	if len(list.Secrets[0].UsedBy) != 1 || list.Secrets[0].UsedBy[0] != "deploy" {
		t.Errorf("used_by = %v", list.Secrets[0].UsedBy)
	}
	var run RunJSON
	json.Unmarshal(dev.expect(202, "POST", "/v1/pipelines/deploy/run", nil), &run)
	if run.Trigger != "ui" || run.TriggeredBy != "dev" || run.PipelineID != "deploy" || run.PipelineVersion != 1 {
		t.Errorf("run = %+v", run)
	}
	h.waitStatus(run.ID, "succeeded")
	mu.Lock()
	if got["DEPLOY_KEY"] != value {
		t.Errorf("the step received %v", got)
	}
	mu.Unlock()
	_, logBytes := h.do("GET", "/v1/runs/"+run.ID+"/steps/ship/logs", testToken, nil, "")
	if strings.Contains(string(logBytes), value) || !strings.Contains(string(logBytes), "shipping with ***") {
		t.Errorf("stored log = %q, want the value masked", logBytes)
	}

	// Rotation bumps the version and takes effect on the next run.
	admin.expect(200, "PUT", "/v1/secrets/DEPLOY_KEY", map[string]string{"value": "rotated-deploy-key-456"})
	json.Unmarshal(dev.expect(200, "GET", "/v1/secrets", nil), &list)
	if list.Secrets[0].Version != 2 {
		t.Errorf("version after rotation = %d", list.Secrets[0].Version)
	}
	json.Unmarshal(dev.expect(202, "POST", "/v1/pipelines/deploy/run", nil), &run)
	h.waitStatus(run.ID, "succeeded")
	mu.Lock()
	if got["DEPLOY_KEY"] != "rotated-deploy-key-456" {
		t.Errorf("after rotation the step received %q", got["DEPLOY_KEY"])
	}
	mu.Unlock()

	// The audit log says who, never what.
	var audit struct{ Events []AuditEvent }
	json.Unmarshal(admin.expect(200, "GET", "/v1/audit?action=secret", nil), &audit)
	var actions []string
	for _, e := range audit.Events {
		actions = append(actions, e.Action)
		if strings.Contains(e.Detail, "deploy-key") {
			t.Errorf("audit detail contains secret material: %+v", e)
		}
	}
	if !strings.Contains(strings.Join(actions, ","), "secret.rotate") || !strings.Contains(strings.Join(actions, ","), "secret.create") {
		t.Errorf("audit actions = %v", actions)
	}

	admin.expect(204, "DELETE", "/v1/secrets/DEPLOY_KEY", nil)
	admin.expect(404, "DELETE", "/v1/secrets/DEPLOY_KEY", nil)
	// With the secret gone, the run fails by name rather than starting the step.
	json.Unmarshal(dev.expect(202, "POST", "/v1/pipelines/deploy/run", nil), &run)
	if r := h.waitStatus(run.ID, "failed"); !strings.Contains(r.Steps[1].Error, "DEPLOY_KEY") {
		t.Errorf("failed step error = %q", r.Steps[1].Error)
	}
}

func TestEnvironmentSecretsWinAndCannotBeEdited(t *testing.T) {
	h := newHarness(t, &executor.Fake{}, func(c *Config) { c.Secrets = map[string]string{"FROM_ENV": "environment-value-1"} })
	admin := h.adminBrowser("ada")
	admin.expect(409, "PUT", "/v1/secrets/FROM_ENV", map[string]string{"value": "attempted-override-1"})
	admin.expect(409, "DELETE", "/v1/secrets/FROM_ENV", nil)
	var list struct{ Secrets []SecretJSON }
	json.Unmarshal(admin.expect(200, "GET", "/v1/secrets", nil), &list)
	if len(list.Secrets) != 1 || list.Secrets[0].Source != "environment" {
		t.Fatalf("list = %+v", list)
	}
}

func TestInternalSecretsCannotBeRequestedOrManaged(t *testing.T) {
	h := newHarness(t, &executor.Fake{})
	admin := h.adminBrowser("ada")
	if _, _, err := h.srv.putSecret(context.Background(), "test", "forgeline.github.token", "ghp_internal_token_value", ""); err != nil {
		t.Fatal(err)
	}
	admin.expect(400, "PUT", "/v1/secrets/forgeline.github.token", map[string]string{"value": "attacker-supplied-value"})
	admin.expect(404, "DELETE", "/v1/secrets/forgeline.github.token", nil)
	var list struct{ Secrets []SecretJSON }
	json.Unmarshal(admin.expect(200, "GET", "/v1/secrets", nil), &list)
	if len(list.Secrets) != 0 {
		t.Errorf("internal secret listed: %+v", list)
	}
	if _, ok := h.srv.stepSecrets(context.Background())["forgeline.github.token"]; ok {
		t.Fatal("an internal secret is available to steps")
	}
	// A pipeline cannot even name one.
	var v ValidateJSON
	json.Unmarshal(admin.expect(200, "POST", "/v1/pipelines/validate", map[string]string{"yaml": strings.Replace(withSecret, "DEPLOY_KEY", "forgeline.github.token", 1)}), &v)
	if v.Valid {
		t.Error("a pipeline requesting an internal secret validated")
	}
}

func TestOverviewSummarisesPipelines(t *testing.T) {
	fake := &executor.Fake{Handler: func(_ context.Context, s executor.StepRun, _ io.Writer) (executor.Result, error) {
		if s.ID == "test" && strings.Contains(s.Script, "-v") {
			return executor.Result{ExitCode: 1}, nil
		}
		return executor.Result{}, nil
	}}
	h := newHarness(t, fake)
	admin := h.adminBrowser("ada")
	admin.expect(201, "POST", "/v1/pipelines", map[string]any{"yaml": twoSteps, "description": "demo build"})
	admin.expect(201, "POST", "/v1/pipelines", map[string]any{"yaml": strings.Replace(withSecret, "DEPLOY_KEY", "NOT_THERE", 1)})

	var ov OverviewJSON
	json.Unmarshal(admin.expect(200, "GET", "/v1/overview", nil), &ov)
	if len(ov.Pipelines) != 2 || ov.Pipelines[0].Latest != nil || !ov.Pipelines[0].Saved {
		t.Fatalf("before any run: %+v", ov.Pipelines)
	}

	var run RunJSON
	json.Unmarshal(admin.expect(202, "POST", "/v1/pipelines/demo/run", nil), &run)
	h.waitStatus(run.ID, "succeeded")
	admin.expect(200, "PUT", "/v1/pipelines/demo", map[string]any{"yaml": strings.Replace(twoSteps, "make test", "make test -v", 1), "description": "demo build"})
	json.Unmarshal(admin.expect(202, "POST", "/v1/pipelines/demo/run", nil), &run)
	h.waitStatus(run.ID, "failed")

	json.Unmarshal(admin.expect(200, "GET", "/v1/overview", nil), &ov)
	var demo *PipelineSummary
	for i := range ov.Pipelines {
		if ov.Pipelines[i].Name == "demo" {
			demo = &ov.Pipelines[i]
		}
	}
	if demo == nil || demo.Latest == nil || demo.Latest.Status != "failed" || demo.Latest.TriggeredBy != "ada" {
		t.Fatalf("demo = %+v", demo)
	}
	if len(demo.Recent) != 2 || demo.Recent[0] != "succeeded" || demo.Recent[1] != "failed" {
		t.Errorf("recent = %v, want oldest first", demo.Recent)
	}
	if demo.SuccessRate == nil || *demo.SuccessRate != 0.5 || demo.AvgSeconds == nil {
		t.Errorf("rate = %v, avg = %v", demo.SuccessRate, demo.AvgSeconds)
	}
	if ov.Pipelines[0].Name != "demo" {
		t.Errorf("the failing pipeline should sort first, got %s", ov.Pipelines[0].Name)
	}
	if ov.Totals.Succeeded != 1 || ov.Totals.Failed != 1 || ov.Totals.Window != 2 || len(ov.Recent) != 2 {
		t.Errorf("totals = %+v, recent = %d", ov.Totals, len(ov.Recent))
	}

	var list ListJSON
	json.Unmarshal(admin.expect(200, "GET", "/v1/runs?pipeline=demo", nil), &list)
	if len(list.Runs) != 2 {
		t.Errorf("filtered runs = %d, want 2", len(list.Runs))
	}
	json.Unmarshal(admin.expect(200, "GET", "/v1/runs?pipeline=nothing", nil), &list)
	if len(list.Runs) != 0 {
		t.Errorf("filter for an unknown pipeline returned %d runs", len(list.Runs))
	}
}

func TestRunsStartedWithTheBearerTokenAreAttributed(t *testing.T) {
	h := newHarness(t, &executor.Fake{})
	body, ct := multipartBody(t, twoSteps, nil)
	resp, b := h.do("POST", "/v1/runs", testToken, body, ct)
	var run RunJSON
	json.Unmarshal(b, &run)
	if resp.StatusCode != 202 || run.TriggeredBy != "token" {
		t.Fatalf("status %d, triggered_by %q", resp.StatusCode, run.TriggeredBy)
	}
}

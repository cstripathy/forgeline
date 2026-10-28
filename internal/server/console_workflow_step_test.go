package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forgelinehq/forgeline/internal/executor"
	"github.com/forgelinehq/forgeline/internal/store"
)

// wfGH stands in for the GitHub calls a workflow step makes. A started run goes queued, in
// progress, then completed with the conclusion set in the test.
type wfGH struct {
	srv        *httptest.Server
	mu         sync.Mutex
	hits       []string
	bodies     map[string]string
	auth       map[string]string
	conclusion string
	never      bool // never complete
	named      bool // the dispatch answer names the run (200) instead of 204
	polls      int
	started    bool
}

func newWFGH(t *testing.T) *wfGH {
	g := &wfGH{bodies: map[string]string{}, auth: map[string]string{}, conclusion: "success", named: true}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		g.mu.Lock()
		defer g.mu.Unlock()
		g.hits = append(g.hits, key)
		g.bodies[key] = string(b)
		g.auth[key] = r.Header.Get("Authorization")
		switch {
		case strings.HasSuffix(r.URL.Path, "/dispatches"):
			g.started = true
			if g.named {
				io.WriteString(w, `{"workflow_run_id":801,"run_url":"x","html_url":"https://github.com/acme/widgets/actions/runs/801"}`)
				return
			}
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/workflows/ci.yml/runs"):
			runs := `{"id":700,"status":"completed","conclusion":"success"}`
			if g.started {
				runs = `{"id":801,"status":"queued","html_url":"https://github.com/acme/widgets/actions/runs/801"},` + runs
			}
			io.WriteString(w, `{"workflow_runs":[`+runs+`]}`)
		case r.URL.Path == "/repos/acme/widgets/actions/runs/801":
			g.polls++
			st, concl := "completed", g.conclusion
			switch {
			case g.never || g.polls < 2:
				st, concl = "queued", ""
			case g.polls < 3:
				st, concl = "in_progress", ""
			}
			io.WriteString(w, `{"id":801,"status":"`+st+`","conclusion":"`+concl+`","html_url":"https://github.com/acme/widgets/actions/runs/801"}`)
		case strings.HasSuffix(r.URL.Path, "/801/cancel"):
			w.WriteHeader(202)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *wfGH) count(substr string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, h := range g.hits {
		if strings.Contains(h, substr) {
			n++
		}
	}
	return n
}

func wfPipeline(extra string) string {
	return "version: v1alpha1\nname: release\nsteps:\n  - id: ci\n    workflow:\n      repo: acme/widgets\n      workflow: ci.yml\n      ref: main\n      inputs:\n        env: staging\n" + extra
}

func wfHarness(t *testing.T) (*harness, *wfGH) {
	t.Helper()
	old := workflowPoll
	workflowPoll = 15 * time.Millisecond
	t.Cleanup(func() { workflowPoll = old })
	g := newWFGH(t)
	h := newHarness(t, &executor.Fake{})
	admin := h.adminBrowser("ada")
	admin.expect(200, "PUT", "/v1/integrations/github", map[string]any{
		"enabled": true, "api_base": g.srv.URL, "token": ghToken, "generate_webhook_secret": true,
		"rules": []map[string]any{{"repo": ghRepo, "branches": []string{"main"}}},
	})
	return h, g
}

// stepError is why a step failed. A run's own reason is only a summary.
func stepError(r RunJSON, step string) string {
	for _, st := range r.Steps {
		if st.ID == step {
			return st.Error
		}
	}
	return ""
}

func (h *harness) stepLog(runID, step string) string {
	_, b := h.do("GET", "/v1/runs/"+runID+"/steps/"+step+"/logs", testToken, nil, "")
	return string(b)
}

func TestWorkflowStepStartsTheWorkflowAndWaitsForIt(t *testing.T) {
	h, g := wfHarness(t)
	_, run := h.submit(wfPipeline(""), nil)
	got := h.waitStatus(run.ID, "succeeded")
	if stepStatuses(got)["ci"] != "succeeded" {
		t.Fatalf("steps = %v", stepStatuses(got))
	}
	body := g.bodies["POST /repos/acme/widgets/actions/workflows/ci.yml/dispatches"]
	if !strings.Contains(body, `"ref":"main"`) || !strings.Contains(body, `"env":"staging"`) {
		t.Errorf("GitHub got %s", body)
	}
	if g.auth["POST /repos/acme/widgets/actions/workflows/ci.yml/dispatches"] != "Bearer "+ghToken {
		t.Error("GitHub was not called with the server's token")
	}
	log := h.stepLog(run.ID, "ci")
	for _, want := range []string{"Started ci.yml on main", "Run 801", "Workflow succeeded"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, ghToken) {
		t.Error("the token is in the log")
	}
	admin := h.login("ada")
	if !strings.Contains(string(admin.expect(200, "GET", "/v1/audit", nil)), "actions.step") {
		t.Error("the start is not in the audit log")
	}
}

func TestWorkflowStepFailsWhenTheWorkflowDoes(t *testing.T) {
	h, g := wfHarness(t)
	g.conclusion = "failure"
	_, run := h.submit(wfPipeline(""), nil)
	got := h.waitStatus(run.ID, "failed")
	if !strings.Contains(stepError(got, "ci"), `conclusion "failure"`) {
		t.Errorf("error = %q", stepError(got, "ci"))
	}
}

func TestWorkflowStepFindsTheRunWhenDispatchDoesNotNameIt(t *testing.T) {
	h, g := wfHarness(t)
	g.named = false
	_, run := h.submit(wfPipeline(""), nil)
	h.waitStatus(run.ID, "succeeded")
	if !strings.Contains(h.stepLog(run.ID, "ci"), "Run 801") {
		t.Error("it did not pick the new run (801) over the old one (700)")
	}
}

func TestWorkflowStepDetachDoesNotWait(t *testing.T) {
	h, g := wfHarness(t)
	_, run := h.submit(wfPipeline("      detach: true\n"), nil)
	h.waitStatus(run.ID, "succeeded")
	if g.count("/actions/runs/801") != 0 || g.count("/dispatches") != 1 {
		t.Errorf("hits = %v", g.hits)
	}
}

func TestWorkflowStepIsRefusedOutsideTheAllowListAndBranchRules(t *testing.T) {
	h, g := wfHarness(t)
	for name, yaml := range map[string]string{
		"another repository": strings.Replace(wfPipeline(""), "acme/widgets", "evil/repo", 1),
		"another branch":     strings.Replace(wfPipeline(""), "ref: main", "ref: dev", 1),
	} {
		_, run := h.submit(yaml, nil)
		got := h.waitStatus(run.ID, "failed")
		if !strings.Contains(stepError(got, "ci"), "not on the GitHub allow list") && !strings.Contains(stepError(got, "ci"), "is not allowed") {
			t.Errorf("%s: error = %q", name, stepError(got, "ci"))
		}
	}
	if g.count("/dispatches") != 0 {
		t.Errorf("a refused step reached GitHub: %v", g.hits)
	}
	if !strings.Contains(string(h.login("ada").expect(200, "GET", "/v1/audit", nil)), "refused") {
		t.Error("the refusal is not in the audit log")
	}
}

func TestWorkflowStepIsNotAllowedInRunsStartedByGitHub(t *testing.T) {
	h, g := wfHarness(t)
	r, err := h.store.CreateRun(context.Background(), store.Run{PipelineName: "release", PipelineYAML: wfPipeline(""), Trigger: store.TriggerGitHub}, []string{"ci"})
	if err != nil {
		t.Fatal(err)
	}
	got := h.waitStatus(r.ID, "failed")
	if !strings.Contains(stepError(got, "ci"), "started by GitHub may not start") || g.count("/dispatches") != 0 {
		t.Errorf("error = %q, hits = %v", stepError(got, "ci"), g.hits)
	}
}

func TestWorkflowStepCancelsTheGitHubRunWhenTheRunIsCanceledOrTimesOut(t *testing.T) {
	h, g := wfHarness(t)
	g.never = true
	_, run := h.submit(wfPipeline(""), nil)
	h.waitFor("the step to be waiting", func() bool { return g.count("GET /repos/acme/widgets/actions/runs/801") >= 2 })
	h.do("POST", "/v1/runs/"+run.ID+"/cancel", testToken, nil, "")
	h.waitStatus(run.ID, "canceled")
	h.waitFor("GitHub's run to be canceled", func() bool { return g.count("POST /repos/acme/widgets/actions/runs/801/cancel") == 1 })

	g.mu.Lock()
	g.hits = nil
	g.mu.Unlock()
	_, run2 := h.submit(wfPipeline("    timeout: 200ms\n"), nil)
	got := h.waitStatus(run2.ID, "failed")
	h.waitFor("GitHub's run to be canceled after the timeout", func() bool { return g.count("/801/cancel") == 1 })
	if !strings.Contains(stepError(got, "ci"), "timed out") {
		t.Errorf("error = %q", stepError(got, "ci"))
	}
}

func TestWorkflowStepNeedsGitHub(t *testing.T) {
	old := workflowPoll
	workflowPoll = 15 * time.Millisecond
	t.Cleanup(func() { workflowPoll = old })
	h := newHarness(t, &executor.Fake{})
	_, run := h.submit(wfPipeline(""), nil)
	got := h.waitStatus(run.ID, "failed")
	if !strings.Contains(stepError(got, "ci"), "GitHub is not connected") {
		t.Errorf("error = %q", stepError(got, "ci"))
	}
}

func TestWorkflowStepsShareTheStartsPerMinuteLimit(t *testing.T) {
	h, g := wfHarness(t)
	var b strings.Builder
	b.WriteString("version: v1alpha1\nname: burst\nsteps:\n")
	for i := 0; i < 21; i++ {
		b.WriteString("  - id: s" + string(rune('a'+i)) + "\n    workflow: {repo: acme/widgets, workflow: ci.yml, ref: main, detach: true}\n")
	}
	_, run := h.submit(b.String(), nil)
	got := h.waitStatus(run.ID, "failed")
	ok, refused := 0, 0
	for _, st := range got.Steps {
		switch {
		case st.Status == "succeeded":
			ok++
		case strings.Contains(st.Error, "too many workflow starts"):
			refused++
		}
	}
	if ok != 20 || refused != 1 || g.count("/dispatches") != 20 {
		t.Errorf("succeeded=%d refused=%d dispatches=%d", ok, refused, g.count("/dispatches"))
	}
}

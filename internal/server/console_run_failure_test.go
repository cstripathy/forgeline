package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/forgelinehq/forgeline/internal/store"
)

// failedRun stores a run whose first step (build) failed, with a log, and whose later step was skipped.
func (m *mcpHarness) failedRun(yaml string, exit int, errText, log string) string {
	m.t.Helper()
	ctx := context.Background()
	st := m.srv.cfg.Store
	run, err := st.CreateRun(ctx, store.Run{PipelineName: "app", PipelineYAML: yaml, Trigger: store.TriggerAPI}, []string{"build", "ship"})
	if err != nil {
		m.t.Fatal(err)
	}
	st.UpdateRun(ctx, run.ID, store.RunUpdate{Status: store.RunRunning})
	st.UpdateStep(ctx, run.ID, "build", store.StepUpdate{Status: store.StepRunning})
	st.AppendLog(ctx, run.ID, "build", []byte(log))
	st.UpdateStep(ctx, run.ID, "build", store.StepUpdate{Status: store.StepFailed, ExitCode: exit, Error: errText})
	st.UpdateStep(ctx, run.ID, "ship", store.StepUpdate{Status: store.StepSkipped})
	st.UpdateRun(ctx, run.ID, store.RunUpdate{Status: store.RunFailed, Reason: "step build failed"})
	return run.ID
}

func (m *mcpHarness) failure(id string) RunFailure {
	m.t.Helper()
	var f RunFailure
	json.Unmarshal(m.v.expect(200, "GET", "/v1/runs/"+id+"/failure", nil), &f)
	return f
}

func hasCause(f RunFailure, sub string) bool {
	for _, c := range f.Causes {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

const twoStepYAML = "version: v1alpha1\nname: app\nsteps:\n  - id: build\n    image: alpine\n    run: make\n  - id: ship\n    image: alpine\n    run: ship\n    needs: [build]\n"

func TestRunFailureNamesTheFirstFailedStepAndItsLikelyCause(t *testing.T) {
	m := newMCPHarness(t)
	cases := []struct {
		name, errText, log string
		exit               int
		cause              string
	}{
		{"missing command", "exited with code 127", "sh: 1: gotestsum: not found\n", 127, "could not find the command"},
		{"not executable", "", "x\n", 126, "could not be run"},
		{"killed", "", "x\n", 137, "was killed"},
		{"crash", "", "x\n", 139, "segmentation fault"},
		{"timeout", `step "build" timed out after 1m`, "x\n", -1, "time limit"},
		{"pull", "pull access denied for ghost/img", "", 0, "could not be pulled"},
		{"secret", `step "build": preparing secrets: x`, "", 0, "secret the step asks for"},
		{"test failure", "", "ok pkg/a\n--- FAIL: TestX (0.00s)\nFAIL\n", 1, "A test failed"},
		{"permission", "", "cp: cannot create: Permission denied\n", 1, "refused access"},
		{"path", "", "cat: nope.txt: No such file or directory\n", 1, "path does not exist"},
		{"credentials", "", "HTTP 401 Unauthorized\n", 1, "rejected the credentials"},
		{"credentials by number", "", "server answered 401\n", 1, "rejected the credentials"},
		{"plain", "", "something odd\n", 1, "reported failure"},
		{"oom word", "", "kill: out of memory\n", 1, "ran out of memory"},
	}
	for _, c := range cases {
		f := m.failure(m.failedRun(twoStepYAML, c.exit, c.errText, c.log))
		if !f.Failed || f.Step != "build" || len(f.Skipped) != 1 || f.Skipped[0] != "ship" || !hasCause(f, c.cause) {
			t.Errorf("%s: %+v", c.name, f)
		}
		if strings.Contains(f.Headline, "exited with code") {
			t.Errorf("%s: the headline repeats the exit code: %q", c.name, f.Headline)
		}
		if !strings.HasPrefix(f.Headline, "Step build failed") {
			t.Errorf("%s: headline %q", c.name, f.Headline)
		}
	}
}

func TestRunFailureSeesTheNetworkRestriction(t *testing.T) {
	m := newMCPHarness(t)
	log := "curl: (6) Could not resolve host: example.com\n"
	offline := strings.Replace(twoStepYAML, "run: make", "run: make\n    permissions:\n      network: none", 1)
	if f := m.failure(m.failedRun(offline, 6, "", log)); !hasCause(f, "network set to none") {
		t.Errorf("offline: %+v", f)
	}
	if f := m.failure(m.failedRun(twoStepYAML, 6, "", log)); !hasCause(f, "could not reach something") || hasCause(f, "network set to none") {
		t.Errorf("online: %+v", f)
	}
}

func TestRunFailureTailIsShortAndEmptyForGoodRuns(t *testing.T) {
	m := newMCPHarness(t)
	var log strings.Builder
	for i := 0; i < 300; i++ {
		log.WriteString("line number " + strings.Repeat("x", 5) + "\n")
	}
	log.WriteString("the last line\n")
	f := m.failure(m.failedRun(twoStepYAML, 1, "", log.String()))
	if n := strings.Count(f.Tail, "\n") + 1; n > failureTailLines || !strings.HasSuffix(f.Tail, "the last line") {
		t.Errorf("tail has %d lines, ends %q", n, f.Tail[max(0, len(f.Tail)-30):])
	}
	// A tail longer than the byte limit loses its start, not its end.
	long := strings.Repeat("0123456789", 100) + "\n"
	var big strings.Builder
	for i := 0; i < 40; i++ {
		big.WriteString(long)
	}
	big.WriteString("the real error\n")
	g := m.failure(m.failedRun(twoStepYAML, 1, "", big.String()))
	if !strings.HasSuffix(g.Tail, "the real error") || len([]rune(g.Tail)) > 4<<10 {
		t.Errorf("tail of %d runes ends %q", len([]rune(g.Tail)), g.Tail[max(0, len(g.Tail)-20):])
	}
	// A run that did not fail has nothing to explain, and the run is looked up by id.
	ctx := context.Background()
	ok, _ := m.srv.cfg.Store.CreateRun(ctx, store.Run{PipelineName: "app", PipelineYAML: twoStepYAML, Trigger: store.TriggerAPI}, []string{"build"})
	if g := m.failure(ok.ID); g.Failed || len(g.Causes) != 0 || g.Tail != "" {
		t.Errorf("good run: %+v", g)
	}
	m.v.expect(404, "GET", "/v1/runs/ghost/failure", nil)
	m.srv.cfg.Store.UpdateRun(ctx, ok.ID, store.RunUpdate{Status: store.RunRunning})
	m.srv.cfg.Store.UpdateRun(ctx, ok.ID, store.RunUpdate{Status: store.RunFailed, Reason: "server restarted"})
	if g := m.failure(ok.ID); !g.Failed || !strings.Contains(g.Headline, "server restarted") || g.Step != "" {
		t.Errorf("run that failed before a step did: %+v", g)
	}
}

func TestRunFailureBlamesTheStepThatFailedFirst(t *testing.T) {
	m := newMCPHarness(t)
	ctx := context.Background()
	st := m.srv.cfg.Store
	run, _ := st.CreateRun(ctx, store.Run{PipelineName: "app", PipelineYAML: twoStepYAML, Trigger: store.TriggerAPI}, []string{"a", "b"})
	st.UpdateRun(ctx, run.ID, store.RunUpdate{Status: store.RunRunning})
	for _, id := range []string{"a", "b"} {
		st.UpdateStep(ctx, run.ID, id, store.StepUpdate{Status: store.StepRunning})
	}
	st.UpdateStep(ctx, run.ID, "b", store.StepUpdate{Status: store.StepFailed, ExitCode: 2})
	time.Sleep(20 * time.Millisecond)
	st.UpdateStep(ctx, run.ID, "a", store.StepUpdate{Status: store.StepFailed, ExitCode: 1})
	st.UpdateRun(ctx, run.ID, store.RunUpdate{Status: store.RunFailed})
	if f := m.failure(run.ID); f.Step != "b" || f.ExitCode != 2 {
		t.Errorf("blamed %s (exit %d), want b: it failed first", f.Step, f.ExitCode)
	}
}

func TestRunFailureWordsThatContainOomAreNotMemoryProblems(t *testing.T) {
	m := newMCPHarness(t)
	f := m.failure(m.failedRun(twoStepYAML, 1, "", "zoom level wrong in the bedroom test\n"))
	if hasCause(f, "out of memory") {
		t.Errorf("%+v", f)
	}
}

func TestRunFailureSummaryOfAFinishedRunIsKept(t *testing.T) {
	m := newMCPHarness(t)
	id := m.failedRun(twoStepYAML, 1, "", "first output\n")
	before := m.failure(id)
	m.srv.cfg.Store.AppendLog(context.Background(), id, "build", []byte("late line\n"))
	if after := m.failure(id); after.Tail != before.Tail {
		t.Errorf("the summary was worked out again: %q vs %q", after.Tail, before.Tail)
	}
}

func TestExplainFailureBlamesAStepWithATimestampOverOneWithout(t *testing.T) {
	m := newMCPHarness(t)
	earlier := time.Now().Add(-time.Minute)
	steps := []store.Step{
		{StepID: "a", Status: store.StepFailed, ExitCode: 1},
		{StepID: "b", Status: store.StepFailed, ExitCode: 2, FinishedAt: &earlier},
	}
	f := m.srv.explainFailure(context.Background(), store.Run{ID: "r", Status: store.RunFailed, PipelineYAML: twoStepYAML}, steps)
	if f.Step != "b" {
		t.Errorf("blamed %s", f.Step)
	}
}

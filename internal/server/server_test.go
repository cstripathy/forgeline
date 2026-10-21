package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cstripathy/forgeline/internal/executor"
	"github.com/cstripathy/forgeline/internal/store"
)

const testToken = "test-token-0123456789"

const twoSteps = `version: v1alpha1
name: demo
steps:
  - id: build
    image: alpine
    run: make
  - id: test
    image: alpine
    run: make test
    needs: [build]
`

type harness struct {
	t     *testing.T
	srv   *Server
	http  *httptest.Server
	store store.Store
	fake  *executor.Fake
}

func newHarness(t *testing.T, fake *executor.Fake, mutate ...func(*Config)) *harness {
	t.Helper()
	st := store.NewMemory()
	cfg := Config{Store: st, Executor: fake, Token: testToken, DataDir: t.TempDir(), MaxRuns: 1,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, m := range mutate {
		m(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); srv.Stop() })
	return &harness{t: t, srv: srv, http: ts, store: st, fake: fake}
}

func (h *harness) do(method, path, token string, body io.Reader, contentType string) (*http.Response, []byte) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.http.URL+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func multipartBody(t *testing.T, yaml string, source []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if yaml != "" {
		fw, _ := mw.CreateFormField("pipeline")
		fw.Write([]byte(yaml))
	}
	if source != nil {
		fw, _ := mw.CreateFormFile("source", "source.tar.gz")
		fw.Write(source)
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func (h *harness) submit(yaml string, source []byte) (*http.Response, RunJSON) {
	h.t.Helper()
	body, ct := multipartBody(h.t, yaml, source)
	resp, b := h.do("POST", "/v1/runs", testToken, body, ct)
	var run RunJSON
	json.Unmarshal(b, &run)
	return resp, run
}

func (h *harness) get(id string) RunJSON {
	h.t.Helper()
	_, b := h.do("GET", "/v1/runs/"+id, testToken, nil, "")
	var run RunJSON
	if err := json.Unmarshal(b, &run); err != nil {
		h.t.Fatalf("bad run JSON %q: %v", b, err)
	}
	return run
}

// waitFor polls until cond is true or the deadline passes.
func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) waitStatus(id, status string) RunJSON {
	h.t.Helper()
	h.waitFor("run "+id+" to be "+status, func() bool { return h.get(id).Status == status })
	return h.get(id)
}

func stepStatuses(r RunJSON) map[string]string {
	m := map[string]string{}
	for _, s := range r.Steps {
		m[s.ID] = s.Status
	}
	return m
}

func TestAuth(t *testing.T) {
	h := newHarness(t, &executor.Fake{})
	if resp, _ := h.do("GET", "/healthz", "", nil, ""); resp.StatusCode != 200 {
		t.Errorf("healthz = %d, want 200 without a token", resp.StatusCode)
	}
	for name, tok := range map[string]string{"none": "", "wrong": "nope", "prefix": testToken[:len(testToken)-1]} {
		resp, _ := h.do("GET", "/v1/runs", tok, nil, "")
		if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s token: status %d, want 401 with WWW-Authenticate", name, resp.StatusCode)
		}
	}
	// A bare token without the Bearer scheme must not work.
	req, _ := http.NewRequest("GET", h.http.URL+"/v1/runs", nil)
	req.Header.Set("Authorization", testToken)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Errorf("token without Bearer scheme: %d, want 401", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/v1/runs", testToken, nil, ""); resp.StatusCode != 200 {
		t.Errorf("valid token: %d, want 200", resp.StatusCode)
	}
}

func TestNewRejectsShortToken(t *testing.T) {
	_, err := New(Config{Store: store.NewMemory(), Executor: &executor.Fake{}, Token: "short", DataDir: t.TempDir()})
	if err == nil {
		t.Fatal("New accepted a short token")
	}
}

func TestSubmitRejectsInvalidPipelines(t *testing.T) {
	h := newHarness(t, &executor.Fake{})
	tests := map[string]string{
		"validation problems": "version: v1alpha1\nname: Bad Name\nsteps: []\n",
		"syntax error":        "version: [unclosed\n",
		"unknown field":       strings.Replace(twoSteps, "make\n", "make\n    bogus: 1\n", 1),
		"cycle":               "version: v1alpha1\nname: c\nsteps:\n- {id: a, image: i, run: r, needs: [b]}\n- {id: b, image: i, run: r, needs: [a]}\n",
	}
	for name, yaml := range tests {
		resp, _ := h.submit(yaml, nil)
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	body, ct := multipartBody(t, "version: v1alpha1\nname: Bad Name\nsteps: []\n", nil)
	_, b := h.do("POST", "/v1/runs", testToken, body, ct)
	var e ErrorJSON
	json.Unmarshal(b, &e)
	if len(e.Problems) < 2 || e.Problems[0].Path == "" {
		t.Errorf("problems = %+v, want field-path problems", e.Problems)
	}
	if resp, _ := h.do("POST", "/v1/runs", testToken, strings.NewReader("{}"), "application/json"); resp.StatusCode != 400 {
		t.Errorf("non-multipart body: %d, want 400", resp.StatusCode)
	}
	body, ct = multipartBody(t, "", []byte("x"))
	if resp, _ := h.do("POST", "/v1/runs", testToken, body, ct); resp.StatusCode != 400 {
		t.Errorf("missing pipeline: %d, want 400", resp.StatusCode)
	}
	if runs, _ := h.store.ListRuns(context.Background(), store.ListOptions{}); len(runs) != 0 {
		t.Errorf("%d runs created by invalid submissions, want 0", len(runs))
	}
}

func TestRunSucceedsAndStoresLogs(t *testing.T) {
	fake := &executor.Fake{Handler: func(_ context.Context, s executor.StepRun, logs io.Writer) (executor.Result, error) {
		io.WriteString(logs, "hello from "+s.ID+"\n")
		return executor.Result{}, nil
	}}
	h := newHarness(t, fake)
	resp, run := h.submit(twoSteps, nil)
	if resp.StatusCode != 202 || run.ID == "" || (run.Status != "queued" && run.Status != "running") || resp.Header.Get("Location") != "/v1/runs/"+run.ID {
		t.Fatalf("submit = %d %+v", resp.StatusCode, run)
	}
	done := h.waitStatus(run.ID, "succeeded")
	if st := stepStatuses(done); st["build"] != "succeeded" || st["test"] != "succeeded" {
		t.Fatalf("steps = %v", st)
	}
	if done.StartedAt == nil || done.FinishedAt == nil || done.Trigger != "api" {
		t.Errorf("run = %+v", done)
	}
	_, log := h.do("GET", "/v1/runs/"+run.ID+"/steps/build/logs", testToken, nil, "")
	if string(log) != "hello from build\n" {
		t.Errorf("log = %q", log)
	}
	if resp, _ := h.do("GET", "/v1/runs/"+run.ID+"/steps/nope/logs", testToken, nil, ""); resp.StatusCode != 404 {
		t.Errorf("unknown step logs: %d, want 404", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/v1/runs/nope", testToken, nil, ""); resp.StatusCode != 404 {
		t.Errorf("unknown run: %d, want 404", resp.StatusCode)
	}
}

func TestFailureSkipsDependents(t *testing.T) {
	fake := &executor.Fake{Handler: func(_ context.Context, s executor.StepRun, _ io.Writer) (executor.Result, error) {
		if s.ID == "build" {
			return executor.Result{ExitCode: 3}, nil
		}
		return executor.Result{}, nil
	}}
	h := newHarness(t, fake)
	_, run := h.submit(twoSteps, nil)
	done := h.waitStatus(run.ID, "failed")
	st := stepStatuses(done)
	if st["build"] != "failed" || st["test"] != "skipped" {
		t.Fatalf("steps = %v", st)
	}
	if !strings.Contains(done.Reason, "build (failed)") {
		t.Errorf("reason = %q, want it to name the failed step", done.Reason)
	}
	if done.Steps[0].ExitCode != 3 || done.Steps[0].Error == "" {
		t.Errorf("build step = %+v, want exit code 3 and an error", done.Steps[0])
	}
}

func blockingFake(started chan<- string) *executor.Fake {
	return &executor.Fake{Handler: func(ctx context.Context, s executor.StepRun, _ io.Writer) (executor.Result, error) {
		started <- s.ID
		<-ctx.Done()
		return executor.Result{}, ctx.Err()
	}}
}

func TestCancelRunningRunIsIdempotent(t *testing.T) {
	started := make(chan string, 4)
	h := newHarness(t, blockingFake(started))
	_, run := h.submit(twoSteps, nil)
	<-started // build is running

	resp, got := cancel(h, run.ID)
	if resp.StatusCode != 202 {
		t.Errorf("cancel running = %d, want 202", resp.StatusCode)
	}
	_ = got
	done := h.waitStatus(run.ID, "canceled")
	if st := stepStatuses(done); st["build"] != "canceled" || st["test"] != "skipped" && st["test"] != "canceled" {
		t.Errorf("steps = %v", st)
	}
	if resp, _ := cancel(h, run.ID); resp.StatusCode != 200 {
		t.Errorf("cancel finished run = %d, want 200", resp.StatusCode)
	}
	if resp, _ := h.do("POST", "/v1/runs/nope/cancel", testToken, nil, ""); resp.StatusCode != 404 {
		t.Errorf("cancel unknown run = %d, want 404", resp.StatusCode)
	}
}

func cancel(h *harness, id string) (*http.Response, RunJSON) {
	h.t.Helper()
	resp, b := h.do("POST", "/v1/runs/"+id+"/cancel", testToken, nil, "")
	var run RunJSON
	json.Unmarshal(b, &run)
	return resp, run
}

func TestCancelQueuedRun(t *testing.T) {
	started := make(chan string, 4)
	h := newHarness(t, blockingFake(started)) // MaxRuns 1: the second run must wait
	_, first := h.submit(twoSteps, nil)
	<-started
	_, second := h.submit(twoSteps, nil)
	if got := h.get(second.ID).Status; got != "queued" {
		t.Fatalf("second run = %s, want queued behind the first", got)
	}
	resp, c := cancel(h, second.ID)
	if resp.StatusCode != 200 || c.Status != "canceled" {
		t.Fatalf("cancel queued = %d %+v", resp.StatusCode, c)
	}
	for _, st := range c.Steps {
		if st.Status != "canceled" {
			t.Errorf("step %s = %s, want canceled", st.ID, st.Status)
		}
	}
	cancel(h, first.ID)
	h.waitStatus(first.ID, "canceled")
	// The canceled queued run must never start.
	time.Sleep(50 * time.Millisecond)
	if got := h.get(second.ID).Status; got != "canceled" {
		t.Errorf("second run = %s after the first finished, want canceled", got)
	}
}

func TestSourceUploadReachesTheStep(t *testing.T) {
	var seen string
	fake := &executor.Fake{Handler: func(_ context.Context, s executor.StepRun, _ io.Writer) (executor.Result, error) {
		b, err := os.ReadFile(filepath.Join(s.Workspace, "src", "a.txt"))
		if err != nil {
			return executor.Result{ExitCode: 1}, nil
		}
		seen = string(b)
		return executor.Result{}, nil
	}}
	h := newHarness(t, fake)
	arc := makeArchive(t, entry{name: "src/a.txt", body: "from upload"})
	_, run := h.submit("version: v1alpha1\nname: u\nsteps:\n- {id: a, image: i, run: r}\n", arc)
	h.waitStatus(run.ID, "succeeded")
	if seen != "from upload" {
		t.Fatalf("step saw %q, want the uploaded file", seen)
	}
}

func TestHostileUploadIsRejectedAndCreatesNoRun(t *testing.T) {
	h := newHarness(t, &executor.Fake{})
	arc := makeArchive(t, entry{name: "../escape.txt", body: "x"})
	resp, _ := h.submit(twoSteps, arc)
	if resp.StatusCode != 400 {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if runs, _ := h.store.ListRuns(context.Background(), store.ListOptions{}); len(runs) != 0 {
		t.Errorf("%d runs created, want 0", len(runs))
	}
	if entries, _ := os.ReadDir(filepath.Join(h.srv.cfg.DataDir, "tmp")); len(entries) != 0 {
		t.Errorf("scratch space not cleaned up: %d entries left", len(entries))
	}
}

func TestOversizedUploadIsRejected(t *testing.T) {
	h := newHarness(t, &executor.Fake{}, func(c *Config) { c.MaxSourceBytes = 1024 })
	arc := makeArchive(t, entry{name: "big.bin", body: string(make([]byte, 4096))})
	resp, _ := h.submit(twoSteps, arc)
	if resp.StatusCode != 400 && resp.StatusCode != 413 {
		t.Fatalf("status %d, want 400 or 413", resp.StatusCode)
	}
}

func TestListPagination(t *testing.T) {
	h := newHarness(t, &executor.Fake{})
	var ids []string
	for i := 0; i < 3; i++ {
		_, r := h.submit(twoSteps, nil)
		ids = append(ids, r.ID)
		h.waitStatus(r.ID, "succeeded")
	}
	_, b := h.do("GET", "/v1/runs?limit=2", testToken, nil, "")
	var page ListJSON
	json.Unmarshal(b, &page)
	if len(page.Runs) != 2 || page.Runs[0].ID != ids[2] || page.NextBefore != ids[1] {
		t.Fatalf("page 1 = %+v", page)
	}
	_, b = h.do("GET", "/v1/runs?limit=2&before="+page.NextBefore, testToken, nil, "")
	page = ListJSON{} // Unmarshal keeps fields absent from the new body
	json.Unmarshal(b, &page)
	if len(page.Runs) != 1 || page.Runs[0].ID != ids[0] || page.NextBefore != "" {
		t.Fatalf("page 2 = %+v", page)
	}
	if resp, _ := h.do("GET", "/v1/runs?limit=abc", testToken, nil, ""); resp.StatusCode != 400 {
		t.Errorf("bad limit = %d, want 400", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/v1/runs?before=nope", testToken, nil, ""); resp.StatusCode != 400 {
		t.Errorf("bad cursor = %d, want 400", resp.StatusCode)
	}
}

func TestStartFailsRunsInterruptedByARestart(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	r, _ := st.CreateRun(ctx, store.Run{PipelineName: "x", PipelineYAML: twoSteps, Trigger: store.TriggerAPI}, []string{"build", "test"})
	if _, err := st.UpdateRun(ctx, r.ID, store.RunUpdate{Status: store.RunRunning}); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Store: st, Executor: &executor.Fake{}, Token: testToken, DataDir: t.TempDir(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	got, _, _ := st.GetRun(ctx, r.ID)
	if got.Status != store.RunFailed || got.Reason != "server restarted" {
		t.Fatalf("run = %s %q, want failed: server restarted", got.Status, got.Reason)
	}
}

func TestStopCancelsRunningRuns(t *testing.T) {
	started := make(chan string, 4)
	h := newHarness(t, blockingFake(started))
	_, run := h.submit(twoSteps, nil)
	<-started
	h.srv.Stop()
	got, _, _ := h.store.GetRun(context.Background(), run.ID)
	if got.Status != store.RunCanceled || got.Reason != "server shutting down" {
		t.Fatalf("run = %s %q, want canceled: server shutting down", got.Status, got.Reason)
	}
}

func TestLogWriterTruncates(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	r, _ := st.CreateRun(ctx, store.Run{PipelineName: "x", PipelineYAML: "y", Trigger: store.TriggerAPI}, []string{"s"})
	w := &logWriter{log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st, ctx: ctx, runID: r.ID, stepID: "s", limit: 10}
	for i := 0; i < 5; i++ {
		if n, err := w.Write([]byte("0123456789")); n != 10 || err != nil {
			t.Fatalf("Write = %d, %v; a full log must never fail the step", n, err)
		}
	}
	chunks, _ := st.ReadLog(ctx, r.ID, "s", 0, 0)
	var all strings.Builder
	for _, c := range chunks {
		all.Write(c.Data)
	}
	if !strings.HasPrefix(all.String(), "0123456789") || !strings.Contains(all.String(), "[log truncated") {
		t.Fatalf("log = %q", all.String())
	}
	if strings.Count(all.String(), "0123456789") != 1 || strings.Count(all.String(), "truncated") != 1 {
		t.Fatalf("log not capped: %q", all.String())
	}
}

package scheduler

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cstripathy/forgeline/internal/executor"
	"github.com/cstripathy/forgeline/internal/pipeline"
)

func plan(t *testing.T, steps ...pipeline.Step) *pipeline.Graph {
	t.Helper()
	g, err := pipeline.Plan(&pipeline.Pipeline{Version: pipeline.VersionV1Alpha1, Name: "p", Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func st(id string, needs ...string) pipeline.Step {
	return pipeline.Step{ID: id, Image: "alpine", Run: "true", Needs: needs}
}

func statuses(r *Report) map[string]Status {
	m := map[string]Status{}
	for _, s := range r.Steps {
		m[s.ID] = s.Status
	}
	return m
}

func failing(ids ...string) *executor.Fake {
	return &executor.Fake{Handler: func(_ context.Context, s executor.StepRun, _ io.Writer) (executor.Result, error) {
		for _, id := range ids {
			if s.ID == id {
				return executor.Result{ExitCode: 2}, nil
			}
		}
		return executor.Result{}, nil
	}}
}

func TestRunSuccess(t *testing.T) {
	g := plan(t, st("a"), st("b", "a"), st("c", "a"), st("d", "b", "c"))
	f := &executor.Fake{}
	r := Run(context.Background(), g, f, Options{Parallelism: 4})
	if !r.OK() || r.Err() != nil {
		t.Fatalf("report = %+v, want all succeeded", r.Steps)
	}
	calls := f.Calls()
	if len(calls) != 4 || calls[0].ID != "a" || calls[3].ID != "d" {
		t.Fatalf("calls = %v, want a first and d last", calls)
	}
}

func TestRunFailureSkipsDependentsOnly(t *testing.T) {
	// a fails; b needs a, c needs b (indirect); x is unrelated.
	g := plan(t, st("a"), st("b", "a"), st("c", "b"), st("x"), st("y", "x"))
	r := Run(context.Background(), g, failing("a"), Options{Parallelism: 2})
	want := map[string]Status{"a": StatusFailed, "b": StatusSkipped, "c": StatusSkipped,
		"x": StatusSucceeded, "y": StatusSucceeded}
	got := statuses(r)
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s = %s, want %s", id, got[id], w)
		}
	}
	if !errors.Is(r.Err(), ErrFailed) {
		t.Fatalf("Err() = %v, want ErrFailed", r.Err())
	}
	for _, s := range r.Steps {
		if s.ID == "a" && s.ExitCode != 2 {
			t.Errorf("a exit code = %d, want 2", s.ExitCode)
		}
	}
}

func TestRunTimeout(t *testing.T) {
	slow := st("slow")
	slow.Timeout = "20ms"
	g := plan(t, slow, st("after", "slow"))
	f := &executor.Fake{Handler: func(ctx context.Context, s executor.StepRun, _ io.Writer) (executor.Result, error) {
		if s.Timeout != 20*time.Millisecond {
			t.Errorf("timeout passed = %v, want 20ms", s.Timeout)
		}
		<-ctx.Done()
		return executor.Result{}, ctx.Err()
	}}
	r := Run(context.Background(), g, f, Options{})
	got := statuses(r)
	if got["slow"] != StatusFailed || got["after"] != StatusSkipped {
		t.Fatalf("statuses = %v, want slow failed and after skipped", got)
	}
	if !errors.Is(r.Steps[0].Err, executor.ErrTimeout) {
		t.Fatalf("slow err = %v, want ErrTimeout", r.Steps[0].Err)
	}
}

func TestRunCancellation(t *testing.T) {
	g := plan(t, st("a"), st("b", "a"), st("c"))
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var once sync.Once
	f := &executor.Fake{Handler: func(ctx context.Context, s executor.StepRun, _ io.Writer) (executor.Result, error) {
		once.Do(func() { close(started) }) // whichever step gets the only slot first
		<-ctx.Done()
		return executor.Result{}, ctx.Err()
	}}
	go func() { <-started; cancel() }()
	r := Run(ctx, g, f, Options{Parallelism: 1})

	for _, s := range r.Steps {
		if s.Status == StatusSucceeded || s.Status == StatusFailed {
			t.Errorf("%s = %s, want canceled or skipped", s.ID, s.Status)
		}
	}
	got := statuses(r)
	if got["a"] != StatusCanceled || got["c"] != StatusCanceled || got["b"] != StatusSkipped {
		t.Fatalf("statuses = %v, want a and c canceled and b skipped", got)
	}
	if r.OK() {
		t.Fatal("OK() = true after cancellation")
	}
}

func TestRunParallelismLimit(t *testing.T) {
	for _, limit := range []int{1, 2, 3} {
		var cur, max atomic.Int32
		f := &executor.Fake{Handler: func(_ context.Context, _ executor.StepRun, _ io.Writer) (executor.Result, error) {
			n := cur.Add(1)
			for {
				m := max.Load()
				if n <= m || max.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			cur.Add(-1)
			return executor.Result{}, nil
		}}
		g := plan(t, st("a"), st("b"), st("c"), st("d"), st("e"), st("f"))
		if r := Run(context.Background(), g, f, Options{Parallelism: limit}); !r.OK() {
			t.Fatalf("limit %d: %v", limit, r.Err())
		}
		if int(max.Load()) != limit {
			t.Errorf("limit %d: max concurrent = %d, want %d", limit, max.Load(), limit)
		}
	}
}

func TestRunPassesStepFieldsAndLogs(t *testing.T) {
	step := pipeline.Step{ID: "a", Image: "img", Command: []string{"go", "test"},
		Env: map[string]string{"K": "v"}, WorkDir: "/src"}
	f := &executor.Fake{Handler: func(_ context.Context, _ executor.StepRun, logs io.Writer) (executor.Result, error) {
		io.WriteString(logs, "hello")
		return executor.Result{}, nil
	}}
	var mu sync.Mutex
	got := map[string]string{}
	w := func(id string) io.Writer {
		return writerFunc(func(p []byte) { mu.Lock(); got[id] += string(p); mu.Unlock() })
	}
	Run(context.Background(), plan(t, step), f, Options{Workspace: "/ws", Logs: w})

	c := f.Calls()[0]
	if c.Image != "img" || c.Command[0] != "go" || c.Env["K"] != "v" || c.WorkDir != "/src" || c.Workspace != "/ws" {
		t.Fatalf("StepRun = %+v", c)
	}
	if got["a"] != "hello" {
		t.Fatalf("logs = %v, want hello for a", got)
	}
}

type writerFunc func([]byte)

func (w writerFunc) Write(p []byte) (int, error) { w(p); return len(p), nil }

func TestRunCachedCountsAsSuccess(t *testing.T) {
	g := plan(t, st("a"), st("b", "a"))
	f := &executor.Fake{Handler: func(_ context.Context, s executor.StepRun, _ io.Writer) (executor.Result, error) {
		return executor.Result{Cached: s.ID == "a"}, nil
	}}
	r := Run(context.Background(), g, f, Options{})
	got := statuses(r)
	if got["a"] != StatusCached || got["b"] != StatusSucceeded {
		t.Fatalf("statuses = %v, want a cached and b succeeded", got)
	}
	if !r.OK() || r.Err() != nil {
		t.Fatalf("a cached step should count as success: %v", r.Err())
	}
}

func TestRunCallbacks(t *testing.T) {
	// a fails, b needs a (skipped), c is independent.
	g := plan(t, st("a"), st("b", "a"), st("c"))
	var mu sync.Mutex
	started := map[string]bool{}
	done := map[string]Status{}
	opts := Options{
		Parallelism: 2,
		OnStepStart: func(id string) { mu.Lock(); started[id] = true; mu.Unlock() },
		OnStepDone: func(r StepResult) {
			mu.Lock()
			defer mu.Unlock()
			if !started[r.ID] && r.Status != StatusSkipped {
				t.Errorf("%s finished (%s) without a start callback", r.ID, r.Status)
			}
			if _, dup := done[r.ID]; dup {
				t.Errorf("%s reported done twice", r.ID)
			}
			done[r.ID] = r.Status
		},
	}
	Run(context.Background(), g, failing("a"), opts)
	want := map[string]Status{"a": StatusFailed, "b": StatusSkipped, "c": StatusSucceeded}
	for id, w := range want {
		if done[id] != w {
			t.Errorf("done[%s] = %s, want %s", id, done[id], w)
		}
	}
	if started["b"] {
		t.Error("skipped step b got a start callback")
	}
}

func withSecrets(s pipeline.Step, names ...string) pipeline.Step {
	s.Permissions.Secrets = names
	return s
}

func TestStepsReceiveOnlyTheSecretsTheyList(t *testing.T) {
	g := plan(t,
		withSecrets(st("deploy"), "DEPLOY_KEY"),
		withSecrets(st("both"), "DEPLOY_KEY", "NPM_TOKEN"),
		st("plain"))
	f := &executor.Fake{}
	r := Run(context.Background(), g, f, Options{Parallelism: 1, Secrets: map[string]string{
		"DEPLOY_KEY": "deploy-key-value", "NPM_TOKEN": "npm-token-value", "UNUSED": "unused-secret-value"}})
	if !r.OK() {
		t.Fatalf("report = %+v", r.Steps)
	}
	got := map[string]map[string]string{}
	for _, c := range f.Calls() {
		got[c.ID] = c.Secrets
	}
	if len(got["deploy"]) != 1 || got["deploy"]["DEPLOY_KEY"] != "deploy-key-value" {
		t.Errorf("deploy got %v, want only DEPLOY_KEY", got["deploy"])
	}
	if len(got["both"]) != 2 {
		t.Errorf("both got %v, want two secrets", got["both"])
	}
	if len(got["plain"]) != 0 {
		t.Errorf("a step that lists no secrets got %v", got["plain"])
	}
}

func TestMissingSecretFailsTheStepByName(t *testing.T) {
	g := plan(t, withSecrets(st("a"), "NOT_SET"), st("b", "a"))
	f := &executor.Fake{}
	r := Run(context.Background(), g, f, Options{Secrets: map[string]string{"OTHER": "other-secret-value"}})
	s := statuses(r)
	if s["a"] != StatusFailed || s["b"] != StatusSkipped {
		t.Fatalf("statuses = %v, want a failed and b skipped", s)
	}
	err := r.Steps[0].Err.Error()
	if !contains(err, `"NOT_SET"`) || !contains(err, "FORGELINE_SECRET_NOT_SET") {
		t.Errorf("error %q should name the secret and how to set it", err)
	}
	if contains(err, "other-secret-value") {
		t.Errorf("error leaks a value: %q", err)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("a step with a missing secret must not start; calls = %v", f.Calls())
	}
}

func TestSecretsDeniedRefusesOnlyStepsThatAskForSecrets(t *testing.T) {
	g := plan(t, withSecrets(st("deploy"), "DEPLOY_KEY"), st("test"))
	f := &executor.Fake{}
	r := Run(context.Background(), g, f, Options{Parallelism: 1,
		Secrets: map[string]string{"DEPLOY_KEY": "deploy-key-value"}, SecretsDenied: "this run is untrusted"})
	s := statuses(r)
	if s["deploy"] != StatusFailed || s["test"] != StatusSucceeded {
		t.Fatalf("statuses = %v, want deploy failed and test succeeded", s)
	}
	for _, res := range r.Steps {
		if res.ID == "deploy" && (!contains(res.Err.Error(), "DEPLOY_KEY") || !contains(res.Err.Error(), "untrusted")) {
			t.Errorf("error %q should name the secret and the reason", res.Err)
		}
	}
	for _, c := range f.Calls() {
		if len(c.Secrets) != 0 {
			t.Errorf("step %s received secrets %v from a denied run", c.ID, c.Secrets)
		}
		if c.ID == "deploy" {
			t.Error("the refused step must not start")
		}
	}
}

func TestLogsMaskEverySecretValue(t *testing.T) {
	const own, other = "own-secret-value", "other-secret-value"
	g := plan(t, withSecrets(st("a"), "OWN"))
	f := &executor.Fake{Handler: func(_ context.Context, s executor.StepRun, logs io.Writer) (executor.Result, error) {
		// One value split across writes, one the step never asked for, and a tail that
		// looks like the start of a value and is only released when the step ends.
		io.WriteString(logs, "token="+own[:6])
		io.WriteString(logs, own[6:]+"\nother="+other+"\nend own-sec")
		return executor.Result{ExitCode: 1}, nil // a failing step must still flush
	}}
	var out syncBuf
	Run(context.Background(), g, f, Options{
		Secrets: map[string]string{"OWN": own, "OTHER": other},
		Logs:    func(string) io.Writer { return &out },
	})
	got := out.String()
	if contains(got, own) || contains(got, other) {
		t.Fatalf("log leaks a secret: %q", got)
	}
	if want := "token=***\nother=***\nend own-sec"; got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

func TestNoSecretsLeavesLogsUntouched(t *testing.T) {
	g := plan(t, st("a"))
	f := &executor.Fake{Handler: func(_ context.Context, _ executor.StepRun, logs io.Writer) (executor.Result, error) {
		io.WriteString(logs, "hello *** world")
		return executor.Result{}, nil
	}}
	var out syncBuf
	Run(context.Background(), g, f, Options{Logs: func(string) io.Writer { return &out }})
	if out.String() != "hello *** world" {
		t.Fatalf("log = %q", out.String())
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  []byte
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = append(s.b, p...)
	return len(p), nil
}
func (s *syncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return string(s.b) }

func contains(s, sub string) bool { return strings.Contains(s, sub) }

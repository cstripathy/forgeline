package scheduler

import (
	"context"
	"errors"
	"io"
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

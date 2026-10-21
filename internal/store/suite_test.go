package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// runSuite is the behaviour every Store must have. Memory and Postgres both run it.
func runSuite(t *testing.T, open func(t *testing.T) Store) {
	tests := map[string]func(t *testing.T, s Store){
		"create and get":              testCreateGet,
		"get missing":                 testGetMissing,
		"list newest first and pages": testList,
		"claim is oldest first":       testClaim,
		"claim is exclusive":          testClaimConcurrent,
		"run transitions":             testRunTransitions,
		"step transitions":            testStepTransitions,
		"logs append and read":        testLogs,
		"logs concurrent writers":     testLogsConcurrent,
		"fail interrupted runs":       testFailInterrupted,
	}
	for name, fn := range tests {
		t.Run(name, func(t *testing.T) { fn(t, open(t)) })
	}
}

func newRun(t *testing.T, s Store, steps ...string) Run {
	t.Helper()
	if len(steps) == 0 {
		steps = []string{"build", "test"}
	}
	r, err := s.CreateRun(context.Background(), Run{PipelineName: "demo", PipelineYAML: "name: demo\n", Trigger: TriggerAPI}, steps)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func testCreateGet(t *testing.T, s Store) {
	ctx := context.Background()
	in := Run{PipelineName: "demo", PipelineYAML: "yaml text", SourceRepo: "o/r", SourceRef: "refs/heads/main",
		SourceSHA: "abc", Trigger: TriggerGitHub, Status: RunSucceeded}
	r, err := s.CreateRun(ctx, in, []string{"b", "a", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == "" || r.Status != RunQueued || r.CreatedAt.IsZero() || r.StartedAt != nil {
		t.Fatalf("created run = %+v", r)
	}
	got, steps, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PipelineYAML != "yaml text" || got.SourceRepo != "o/r" || got.SourceSHA != "abc" || got.Trigger != TriggerGitHub || got.Status != RunQueued {
		t.Fatalf("got run = %+v", got)
	}
	var ids []string
	for _, st := range steps {
		if st.Status != StepPending || st.RunID != r.ID {
			t.Errorf("step %+v, want pending in run %s", st, r.ID)
		}
		ids = append(ids, st.StepID)
	}
	if fmt.Sprint(ids) != "[b a c]" {
		t.Fatalf("steps = %v, want declaration order [b a c]", ids)
	}
}

func testGetMissing(t *testing.T, s Store) {
	if _, _, err := s.GetRun(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRun = %v, want ErrNotFound", err)
	}
}

func testList(t *testing.T, s Store) {
	ctx := context.Background()
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, newRun(t, s).ID)
	}
	page1, err := s.ListRuns(ctx, ListOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || page1[0].ID != ids[4] || page1[1].ID != ids[3] {
		t.Fatalf("page1 = %v, want newest first %v", runIDs(page1), []string{ids[4], ids[3]})
	}
	page2, err := s.ListRuns(ctx, ListOptions{Limit: 2, Before: page1[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID != ids[2] || page2[1].ID != ids[1] {
		t.Fatalf("page2 = %v", runIDs(page2))
	}
	all, _ := s.ListRuns(ctx, ListOptions{})
	if len(all) != 5 {
		t.Fatalf("default list returned %d runs, want 5", len(all))
	}
	if _, err := s.ListRuns(ctx, ListOptions{Before: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown cursor = %v, want ErrNotFound", err)
	}
}

func runIDs(rs []Run) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

func testClaim(t *testing.T, s Store) {
	ctx := context.Background()
	if _, ok, err := s.ClaimQueuedRun(ctx); err != nil || ok {
		t.Fatalf("empty claim = ok %v err %v, want not ok", ok, err)
	}
	first, second := newRun(t, s), newRun(t, s)
	got, ok, err := s.ClaimQueuedRun(ctx)
	if err != nil || !ok || got.ID != first.ID || got.Status != RunRunning || got.StartedAt == nil {
		t.Fatalf("claim = %+v ok %v err %v, want first run running", got, ok, err)
	}
	got, ok, _ = s.ClaimQueuedRun(ctx)
	if !ok || got.ID != second.ID {
		t.Fatalf("second claim = %+v, want second run", got)
	}
	if _, ok, _ = s.ClaimQueuedRun(ctx); ok {
		t.Fatal("third claim succeeded with nothing queued")
	}
}

func testClaimConcurrent(t *testing.T, s Store) {
	const runs = 8
	for i := 0; i < runs; i++ {
		newRun(t, s)
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				r, ok, err := s.ClaimQueuedRun(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				seen[r.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != runs {
		t.Fatalf("claimed %d distinct runs, want %d", len(seen), runs)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("run %s claimed %d times", id, n)
		}
	}
}

func testRunTransitions(t *testing.T, s Store) {
	ctx := context.Background()
	r := newRun(t, s)
	if _, err := s.UpdateRun(ctx, r.ID, RunUpdate{Status: RunSucceeded}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("queued -> succeeded = %v, want ErrInvalidTransition", err)
	}
	got, err := s.UpdateRun(ctx, r.ID, RunUpdate{Status: RunRunning})
	if err != nil || got.StartedAt == nil || got.FinishedAt != nil {
		t.Fatalf("-> running = %+v, %v", got, err)
	}
	got, err = s.UpdateRun(ctx, r.ID, RunUpdate{Status: RunFailed, Reason: "step build failed"})
	if err != nil || got.FinishedAt == nil || got.Reason != "step build failed" {
		t.Fatalf("-> failed = %+v, %v", got, err)
	}
	if _, err := s.UpdateRun(ctx, r.ID, RunUpdate{Status: RunRunning}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("failed -> running = %v, want ErrInvalidTransition", err)
	}
	if _, err := s.UpdateRun(ctx, "nope", RunUpdate{Status: RunRunning}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run = %v, want ErrNotFound", err)
	}
	// A queued run can be canceled directly.
	q := newRun(t, s)
	if got, err := s.UpdateRun(ctx, q.ID, RunUpdate{Status: RunCanceled}); err != nil || got.Status != RunCanceled {
		t.Fatalf("queued -> canceled = %+v, %v", got, err)
	}
}

func testStepTransitions(t *testing.T, s Store) {
	ctx := context.Background()
	r := newRun(t, s)
	st, err := s.UpdateStep(ctx, r.ID, "build", StepUpdate{Status: StepRunning})
	if err != nil || st.StartedAt == nil || st.FinishedAt != nil {
		t.Fatalf("-> running = %+v, %v", st, err)
	}
	st, err = s.UpdateStep(ctx, r.ID, "build", StepUpdate{Status: StepFailed, ExitCode: 2, Error: "exited with code 2"})
	if err != nil || st.ExitCode != 2 || st.Error != "exited with code 2" || st.FinishedAt == nil {
		t.Fatalf("-> failed = %+v, %v", st, err)
	}
	if _, err := s.UpdateStep(ctx, r.ID, "build", StepUpdate{Status: StepRunning}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("failed -> running = %v, want ErrInvalidTransition", err)
	}
	// Steps that never ran may go straight from pending to skipped or cached.
	for _, to := range []StepStatus{StepSkipped, StepCached} {
		r2 := newRun(t, s)
		if _, err := s.UpdateStep(ctx, r2.ID, "test", StepUpdate{Status: to}); err != nil {
			t.Errorf("pending -> %s: %v", to, err)
		}
	}
	if _, err := s.UpdateStep(ctx, r.ID, "nope", StepUpdate{Status: StepRunning}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing step = %v, want ErrNotFound", err)
	}
	_, steps, _ := s.GetRun(ctx, r.ID)
	if steps[0].Status != StepFailed || steps[1].Status != StepPending {
		t.Fatalf("steps after updates = %+v", steps)
	}
}

func testLogs(t *testing.T, s Store) {
	ctx := context.Background()
	r := newRun(t, s)
	for i, line := range []string{"one\n", "two\n", "three\n"} {
		seq, err := s.AppendLog(ctx, r.ID, "build", []byte(line))
		if err != nil || seq != int64(i+1) {
			t.Fatalf("AppendLog = %d, %v, want %d", seq, err, i+1)
		}
	}
	if seq, _ := s.AppendLog(ctx, r.ID, "test", []byte("other\n")); seq != 1 {
		t.Fatalf("seq for another step = %d, want 1 (sequences are per step)", seq)
	}
	all, err := s.ReadLog(ctx, r.ID, "build", 0, 0)
	if err != nil || len(all) != 3 || string(all[0].Data) != "one\n" || all[2].Seq != 3 {
		t.Fatalf("ReadLog all = %+v, %v", all, err)
	}
	rest, _ := s.ReadLog(ctx, r.ID, "build", 1, 0)
	if len(rest) != 2 || rest[0].Seq != 2 {
		t.Fatalf("ReadLog after 1 = %+v", rest)
	}
	limited, _ := s.ReadLog(ctx, r.ID, "build", 0, 2)
	if len(limited) != 2 {
		t.Fatalf("ReadLog limit 2 returned %d", len(limited))
	}
	empty, err := s.ReadLog(ctx, r.ID, "build", 3, 0)
	if err != nil || len(empty) != 0 {
		t.Fatalf("ReadLog past end = %+v, %v", empty, err)
	}
	if _, err := s.AppendLog(ctx, r.ID, "nope", []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AppendLog to missing step = %v, want ErrNotFound", err)
	}
	if _, err := s.ReadLog(ctx, "nope", "build", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ReadLog missing run = %v, want ErrNotFound", err)
	}
}

func testLogsConcurrent(t *testing.T, s Store) {
	ctx := context.Background()
	r := newRun(t, s)
	const writers, each = 4, 10
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := s.AppendLog(ctx, r.ID, "build", []byte("x")); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	chunks, err := s.ReadLog(ctx, r.ID, "build", 0, 0)
	if err != nil || len(chunks) != writers*each {
		t.Fatalf("got %d chunks, err %v, want %d", len(chunks), err, writers*each)
	}
	for i, c := range chunks {
		if c.Seq != int64(i+1) {
			t.Fatalf("chunk %d has seq %d: sequence has a gap or duplicate", i, c.Seq)
		}
	}
}

func testFailInterrupted(t *testing.T, s Store) {
	ctx := context.Background()
	running := newRun(t, s)
	queued := newRun(t, s)
	done := newRun(t, s)
	for _, id := range []string{running.ID, done.ID} {
		if _, err := s.UpdateRun(ctx, id, RunUpdate{Status: RunRunning}); err != nil {
			t.Fatal(err)
		}
	}
	s.UpdateStep(ctx, running.ID, "build", StepUpdate{Status: StepSucceeded})
	s.UpdateStep(ctx, running.ID, "test", StepUpdate{Status: StepRunning})
	s.UpdateRun(ctx, done.ID, RunUpdate{Status: RunSucceeded})

	n, err := s.FailInterruptedRuns(ctx, "server restarted")
	if err != nil || n != 1 {
		t.Fatalf("FailInterruptedRuns = %d, %v, want 1", n, err)
	}
	r, steps, _ := s.GetRun(ctx, running.ID)
	if r.Status != RunFailed || r.Reason != "server restarted" || r.FinishedAt == nil {
		t.Fatalf("interrupted run = %+v", r)
	}
	if steps[0].Status != StepSucceeded || steps[1].Status != StepCanceled {
		t.Fatalf("steps = %+v, want finished step kept and running step canceled", steps)
	}
	if q, _, _ := s.GetRun(ctx, queued.ID); q.Status != RunQueued {
		t.Fatalf("queued run = %s, want untouched", q.Status)
	}
	if d, _, _ := s.GetRun(ctx, done.ID); d.Status != RunSucceeded {
		t.Fatalf("finished run = %s, want untouched", d.Status)
	}
}

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/forgelinehq/forgeline/internal/executor"
)

// copyExample copies examples/hello into a temp dir and returns the pipeline path.
func copyExample(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "examples", "hello")
	dst := t.TempDir()
	for _, rel := range []string{"pipeline.yaml", "src/a.txt", "src/b.txt"} {
		data, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dst, "pipeline.yaml")
}

// helloFake emulates what the example's shell scripts do to the workspace, so downstream
// input hashes change when upstream inputs change. failID, if set, exits non-zero.
func helloFake(failID string) *executor.Fake {
	return &executor.Fake{Handler: func(_ context.Context, s executor.StepRun, logs io.Writer) (executor.Result, error) {
		io.WriteString(logs, "running "+s.ID+"\n")
		if s.ID == failID {
			return executor.Result{ExitCode: 1}, nil
		}
		read := func(rel string) []byte {
			b, _ := os.ReadFile(filepath.Join(s.Workspace, rel))
			return b
		}
		write := func(rel string, b []byte) {
			p := filepath.Join(s.Workspace, rel)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, b, 0o644)
		}
		switch s.ID {
		case "shout-a":
			write("out/a.txt", bytes.ToUpper(read("src/a.txt")))
		case "shout-b":
			write("out/b.txt", bytes.ToUpper(read("src/b.txt")))
		case "combine":
			write("out/combined.txt", append(read("out/a.txt"), read("out/b.txt")...))
		}
		return executor.Result{}, nil
	}}
}

func runPipelineFile(t *testing.T, ex executor.Executor, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = runWith(append([]string{"run"}, args...), &out, &errb, ex)
	return code, out.String(), errb.String()
}

func statusOf(out, id string) string {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == id || len(f) >= 2 && f[1] == id+":" {
			return f[0]
		}
	}
	return ""
}

func TestRunCachesAndRerunsOnlyAffectedSteps(t *testing.T) {
	file := copyExample(t)
	fake := helloFake("")

	code, out, errs := runPipelineFile(t, fake, file)
	if code != 0 {
		t.Fatalf("first run exit %d, stderr %q", code, errs)
	}
	for _, id := range []string{"shout-a", "shout-b", "combine"} {
		if got := statusOf(out, id); got != "ran" {
			t.Errorf("first run: %s = %q, want ran\n%s", id, got, out)
		}
	}
	if !strings.Contains(out, "[shout-a] running shout-a") {
		t.Errorf("step logs missing or unprefixed:\n%s", out)
	}

	code, out, _ = runPipelineFile(t, fake, file)
	if code != 0 {
		t.Fatalf("second run exit %d", code)
	}
	for _, id := range []string{"shout-a", "shout-b", "combine"} {
		if got := statusOf(out, id); got != "cached" {
			t.Errorf("second run: %s = %q, want cached\n%s", id, got, out)
		}
	}
	if n := len(fake.Calls()); n != 3 {
		t.Errorf("executor ran %d steps in total, want 3", n)
	}

	// Change one input: shout-a and what depends on its output re-run; shout-b stays cached.
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "src", "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out, _ = runPipelineFile(t, fake, file)
	want := map[string]string{"shout-a": "ran", "shout-b": "cached", "combine": "ran"}
	for id, w := range want {
		if got := statusOf(out, id); got != w {
			t.Errorf("after change: %s = %q, want %s\n%s", id, got, w, out)
		}
	}
}

func TestRunNoCacheRunsEverything(t *testing.T) {
	file := copyExample(t)
	fake := helloFake("")
	runPipelineFile(t, fake, file)
	_, out, _ := runPipelineFile(t, fake, "--no-cache", file)
	if got := statusOf(out, "shout-a"); got != "ran" {
		t.Errorf("--no-cache: shout-a = %q, want ran", got)
	}
}

func TestRunFailureSkipsDependentAndExitsNonZero(t *testing.T) {
	file := copyExample(t)
	code, out, errs := runPipelineFile(t, helloFake("shout-a"), file)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	want := map[string]string{"shout-a": "FAILED", "shout-b": "ran", "combine": "skipped"}
	for id, w := range want {
		if got := statusOf(out, id); got != w {
			t.Errorf("%s = %q, want %s\n%s", id, got, w, out)
		}
	}
	if !strings.Contains(errs, "shout-a (failed)") {
		t.Errorf("stderr %q should name the failed step", errs)
	}
	// A failed step must not be cached.
	code, _, _ = runPipelineFile(t, helloFake("shout-a"), file)
	if code != 1 {
		t.Errorf("rerun exit = %d, want 1 (failure must not be cached)", code)
	}
}

func TestRunUsageAndLoadErrors(t *testing.T) {
	if code, _, errs := runPipelineFile(t, &executor.Fake{}); code != 2 || !strings.Contains(errs, "usage") {
		t.Errorf("no file: code %d stderr %q", code, errs)
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(bad, []byte("version: v1alpha1\nname: x\nsteps: []\n"), 0o644)
	if code, _, errs := runPipelineFile(t, &executor.Fake{}, bad); code != 1 || !strings.Contains(errs, "steps:") {
		t.Errorf("invalid file: code %d stderr %q", code, errs)
	}
	cyc := filepath.Join(t.TempDir(), "cycle.yaml")
	os.WriteFile(cyc, []byte("version: v1alpha1\nname: x\nsteps:\n- {id: a, image: i, run: r, needs: [b]}\n- {id: b, image: i, run: r, needs: [a]}\n"), 0o644)
	if code, _, errs := runPipelineFile(t, &executor.Fake{}, cyc); code != 1 || !strings.Contains(errs, "cycle: a -> b -> a") {
		t.Errorf("cycle: code %d stderr %q", code, errs)
	}
}

func TestPrefixLogsKeepsLinesWhole(t *testing.T) {
	var out bytes.Buffer
	l := newPrefixLogs(&out)
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		w := l.writer(id)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				io.WriteString(w, "par")
				io.WriteString(w, "tial line\n")
			}
			io.WriteString(w, "no newline")
		}()
	}
	wg.Wait()
	l.flush()
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line != "[a] partial line" && line != "[b] partial line" && line != "[a] no newline" && line != "[b] no newline" {
			t.Fatalf("bad line %q", line)
		}
	}
}

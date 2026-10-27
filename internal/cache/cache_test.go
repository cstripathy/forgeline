package cache

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cstripathy/forgeline/internal/executor"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"go.mod", "go.mod", true},
		{"go.mod", "sub/go.mod", false},
		{"*.go", "main.go", true},
		{"*.go", "sub/main.go", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "a/b/main.go", true},
		{"**/*.go", "a/b/main.txt", false},
		{"src", "src/a/b.txt", true}, // a directory pattern matches everything below it
		{"src/**", "src/a/b.txt", true},
		{"src/**/x.txt", "src/x.txt", true},
		{"src/**/x.txt", "src/a/b/x.txt", true},
		{"src/[ab]?.txt", "src/a1.txt", true},
		{"src/[ab]?.txt", "src/c1.txt", false},
		{"docs", "docsx/a.txt", false},
	}
	for _, tt := range tests {
		if got := matchGlob(tt.pattern, tt.name); got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestMatchFilesSkipsCacheDirAndSymlinks(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "a.txt", "a")
	write(t, ws, "sub/b.txt", "b")
	write(t, ws, ".forgeline/cache/x.json", "{}")
	outside := t.TempDir()
	write(t, outside, "secret.txt", "s")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(ws, "link.txt")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	got, err := matchFiles(ws, []string{"**"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.txt", "sub/b.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
}

func baseRun(ws string) executor.StepRun {
	return executor.StepRun{ID: "s", Image: "alpine", Script: "true", Workspace: ws, Inputs: []string{"src"}}
}

func TestKeyChangesWithEachComponent(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "src/a.txt", "one")
	write(t, ws, "other/b.txt", "b")
	base, err := Key(baseRun(ws))
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Key(baseRun(ws))
	if again != base {
		t.Fatal("key is not deterministic")
	}

	mutations := map[string]func(*executor.StepRun){
		"image":   func(s *executor.StepRun) { s.Image = "debian" },
		"script":  func(s *executor.StepRun) { s.Script = "false" },
		"command": func(s *executor.StepRun) { s.Script = ""; s.Command = []string{"true"} },
		"env":     func(s *executor.StepRun) { s.Env = map[string]string{"A": "1"} },
		"workdir": func(s *executor.StepRun) { s.WorkDir = "/src" },
		"inputs":  func(s *executor.StepRun) { s.Inputs = []string{"src", "other"} },
		"network": func(s *executor.StepRun) { s.Network = "none" },
	}
	for name, mutate := range mutations {
		m := baseRun(ws)
		mutate(&m)
		if k, _ := Key(m); k == base {
			t.Errorf("changing %s did not change the key", name)
		}
	}

	// A pattern that matches no new files leaves the key alone; the key hashes files read.
	s := baseRun(ws)
	s.Inputs = []string{"src", "nothing-here"}
	if k, _ := Key(s); k != base {
		t.Error("a pattern matching no files changed the key")
	}

	// Changing an input file changes the key; an unrelated file does not.
	write(t, ws, "unrelated.txt", "x")
	if k, _ := Key(baseRun(ws)); k != base {
		t.Error("unrelated file changed the key")
	}
	write(t, ws, "src/a.txt", "two")
	if k, _ := Key(baseRun(ws)); k == base {
		t.Error("changing an input file did not change the key")
	}
}

func TestKeyTreatsDefaultNetworkAsEgress(t *testing.T) {
	ws := t.TempDir()
	a, b := baseRun(ws), baseRun(ws)
	b.Network = "egress"
	ka, _ := Key(a)
	kb, _ := Key(b)
	if ka != kb {
		t.Fatal("an empty network and an explicit egress should hash the same")
	}
}

func TestKeyTreatsDefaultWorkdirAsSame(t *testing.T) {
	ws := t.TempDir()
	a := baseRun(ws)
	b := baseRun(ws)
	b.WorkDir = executor.WorkspaceMount
	ka, _ := Key(a)
	kb, _ := Key(b)
	if ka != kb {
		t.Fatal("empty workdir and /workspace should hash the same")
	}
}

func TestKeyFieldsCannotRunTogether(t *testing.T) {
	ws := t.TempDir()
	a := executor.StepRun{ID: "s", Image: "ab", Script: "c", Workspace: ws}
	b := executor.StepRun{ID: "s", Image: "a", Script: "bc", Workspace: ws}
	ka, _ := Key(a)
	kb, _ := Key(b)
	if ka == kb {
		t.Fatal("different image/script splits produced the same key")
	}
}

func TestExecutorCachesSuccessOnly(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "src/a.txt", "one")
	fake := &executor.Fake{}
	ex := Executor{Next: fake, Store: &Store{Dir: filepath.Join(ws, ".forgeline", "cache")}}
	run := func() executor.Result {
		res, err := ex.Run(context.Background(), baseRun(ws), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	if run().Cached {
		t.Fatal("first run was cached")
	}
	if !run().Cached {
		t.Fatal("second run was not cached")
	}
	if n := len(fake.Calls()); n != 1 {
		t.Fatalf("underlying executor ran %d times, want 1", n)
	}
	write(t, ws, "src/a.txt", "two")
	if run().Cached {
		t.Fatal("run after input change was cached")
	}
}

func TestExecutorDoesNotCacheFailures(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "src/a.txt", "one")
	fake := &executor.Fake{Handler: func(context.Context, executor.StepRun, io.Writer) (executor.Result, error) {
		return executor.Result{ExitCode: 1}, nil
	}}
	ex := Executor{Next: fake, Store: &Store{Dir: filepath.Join(ws, ".forgeline", "cache")}}
	for i := 0; i < 2; i++ {
		res, _ := ex.Run(context.Background(), baseRun(ws), io.Discard)
		if res.Cached {
			t.Fatal("a failed step was served from cache")
		}
	}
	if n := len(fake.Calls()); n != 2 {
		t.Fatalf("ran %d times, want 2", n)
	}
}

func TestExecutorNeverCachesStepsWithoutInputs(t *testing.T) {
	ws := t.TempDir()
	fake := &executor.Fake{}
	ex := Executor{Next: fake, Store: &Store{Dir: filepath.Join(ws, ".forgeline", "cache")}}
	s := baseRun(ws)
	s.Inputs = nil
	for i := 0; i < 2; i++ {
		if res, _ := ex.Run(context.Background(), s, io.Discard); res.Cached {
			t.Fatal("step without inputs was cached")
		}
	}
	if n := len(fake.Calls()); n != 2 {
		t.Fatalf("ran %d times, want 2", n)
	}
}

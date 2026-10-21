package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cstripathy/forgeline/internal/executor"
)

const cliToken = "cli-test-token-0123456789"

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// startServer runs `forgeline server` on a free port and returns its URL.
func startServer(t *testing.T, ex executor.Executor, env map[string]string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var out, errb syncBuffer
	done := make(chan int, 1)
	getenv := func(k string) string { return env[k] }
	go func() {
		done <- runServer(ctx, []string{"--listen", "127.0.0.1:0", "--data-dir", t.TempDir()}, &out, &errb, ex, getenv)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
	})
	re := regexp.MustCompile(`listening on (http://\S+)`)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := re.FindStringSubmatch(out.String()); m != nil {
			return m[1]
		}
		select {
		case code := <-done:
			t.Fatalf("server exited early with %d: %s", code, errb.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server did not start; stderr: %s", errb.String())
	return ""
}

func submit(t *testing.T, url, token string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := map[string]string{"FORGELINE_TOKEN": token, "FORGELINE_SERVER": url}
	code := runSubmit(context.Background(), args, &out, &errb, func(k string) string { return env[k] })
	return code, out.String(), errb.String()
}

func TestServerAndSubmitEndToEnd(t *testing.T) {
	url := startServer(t, helloFake(""), map[string]string{"FORGELINE_TOKEN": cliToken})
	file := copyExample(t)

	code, out, errs := submit(t, url, cliToken, "--wait", file)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	for _, want := range []string{"succeeded", "[shout-a] running shout-a", "combine"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The uploaded workspace was used: the combine step's fake wrote its output there.
	if strings.Contains(out, "FAILED") || strings.Contains(out, "failed") {
		t.Errorf("unexpected failure in output:\n%s", out)
	}
}

func TestSubmitReportsFailureWithExitCode(t *testing.T) {
	url := startServer(t, helloFake("shout-a"), map[string]string{"FORGELINE_TOKEN": cliToken})
	code, out, _ := submit(t, url, cliToken, "--wait", "--no-source", copyExample(t))
	if code != 1 || !strings.Contains(out, "failed") || !strings.Contains(out, "skipped") {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
}

func TestSubmitErrors(t *testing.T) {
	url := startServer(t, &executor.Fake{}, map[string]string{"FORGELINE_TOKEN": cliToken})
	file := copyExample(t)

	if code, _, errs := submit(t, url, "wrong-token-wrong-token", file); code != 1 || !strings.Contains(errs, "401") {
		t.Errorf("wrong token: exit %d, stderr %q", code, errs)
	}
	if code, _, errs := submit(t, url, "", file); code != 2 || !strings.Contains(errs, "FORGELINE_TOKEN") {
		t.Errorf("no token: exit %d, stderr %q", code, errs)
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(bad, []byte("version: v1alpha1\nname: x\nsteps: []\n"), 0o644)
	if code, _, errs := submit(t, url, cliToken, "--no-source", bad); code != 1 || !strings.Contains(errs, "steps:") {
		t.Errorf("invalid pipeline: exit %d, stderr %q (want field paths)", code, errs)
	}
}

func TestServerRefusesWeakConfig(t *testing.T) {
	var out, errb bytes.Buffer
	env := map[string]string{"FORGELINE_TOKEN": "short"}
	code := runServer(context.Background(), []string{"--listen", "127.0.0.1:0", "--data-dir", t.TempDir()},
		&out, &errb, &executor.Fake{}, func(k string) string { return env[k] })
	if code != 1 || !strings.Contains(errb.String(), "token") {
		t.Fatalf("exit %d, stderr %q, want refusal over the token", code, errb.String())
	}
}

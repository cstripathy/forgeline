package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
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
	return startServerWith(t, ex, env)
}

// startServerWith is startServer with extra command-line arguments.
func startServerWith(t *testing.T, ex executor.Executor, env map[string]string, extra ...string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var out, errb syncBuffer
	done := make(chan int, 1)
	getenv := func(k string) string { return env[k] }
	go func() {
		args := append([]string{"--listen", "127.0.0.1:0", "--data-dir", t.TempDir()}, extra...)
		done <- runServer(ctx, args, &out, &errb, ex, getenv)
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

func TestIsLoopbackListen(t *testing.T) {
	tests := map[string]bool{
		"127.0.0.1:8080": true, "localhost:8080": true, "[::1]:8080": true, "127.0.0.1:0": true,
		":8080": false, "0.0.0.0:8080": false, "[::]:8080": false, "10.0.0.5:8080": false, "example.com:80": false, "garbage": false,
	}
	for addr, want := range tests {
		if got := isLoopbackListen(addr); got != want {
			t.Errorf("isLoopbackListen(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestServerGeneratesTokenOnLoopbackOnly(t *testing.T) {
	// Loopback with no token: starts, prints a usable link, and the generated token works.
	ctx, cancel := context.WithCancel(context.Background())
	var out, errb syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- runServer(ctx, []string{"--listen", "127.0.0.1:0", "--data-dir", t.TempDir()}, &out, &errb,
			&executor.Fake{}, func(string) string { return "" })
	}()
	defer func() { cancel(); <-done }()
	re := regexp.MustCompile(`Open:\s+(http://\S+)/#token=(\S+)`)
	var m []string
	for i := 0; i < 500 && m == nil; i++ {
		m = re.FindStringSubmatch(out.String())
		time.Sleep(10 * time.Millisecond)
	}
	if m == nil {
		t.Fatalf("no sign-in link printed; stdout %q stderr %q", out.String(), errb.String())
	}
	if len(m[2]) < 32 {
		t.Errorf("generated token %q is too short", m[2])
	}
	if code, _, errs := submit(t, m[1], m[2], "--no-source", copyExample(t)); code != 0 {
		t.Errorf("submitting with the generated token failed: exit %d, %s", code, errs)
	}

	// A non-loopback address must not invent a token.
	var out2, err2 bytes.Buffer
	code := runServer(context.Background(), []string{"--listen", "0.0.0.0:0", "--data-dir", t.TempDir()}, &out2, &err2,
		&executor.Fake{}, func(string) string { return "" })
	if code != 1 || !strings.Contains(err2.String(), "FORGELINE_TOKEN is required") {
		t.Errorf("non-loopback without token: exit %d, stderr %q", code, err2.String())
	}
}

func TestServerGitHubFlags(t *testing.T) {
	const secret = "webhook-secret-0123456789"
	env := map[string]string{
		"FORGELINE_TOKEN": cliToken, "FORGELINE_GITHUB_WEBHOOK_SECRET": secret, "FORGELINE_GITHUB_TOKEN": "gh-token",
	}
	url := startServerWith(t, &executor.Fake{}, env,
		"--github-allow", "acme/widgets:main,release/*", "--github-allow", "acme/gadgets:main", "--public-url", "https://ci.example.com")

	mac := hmac.New(sha256.New, []byte(secret))
	body := []byte(`{"zen":"x"}`)
	mac.Write(body)
	post := func(sig string) int {
		req, _ := http.NewRequest("POST", url+"/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "ping")
		req.Header.Set("X-Hub-Signature-256", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("sha256=" + hex.EncodeToString(mac.Sum(nil))); got != http.StatusOK {
		t.Errorf("signed ping = %d, want 200", got)
	}
	if got := post("sha256=00"); got != http.StatusUnauthorized {
		t.Errorf("badly signed ping = %d, want 401", got)
	}
}

func TestServerGitHubConfigErrors(t *testing.T) {
	run := func(env map[string]string, args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := runServer(context.Background(), append([]string{"--listen", "127.0.0.1:0", "--data-dir", t.TempDir()}, args...),
			&out, &errb, &executor.Fake{}, func(k string) string { return env[k] })
		return code, errb.String()
	}
	base := map[string]string{"FORGELINE_TOKEN": cliToken}
	with := func(kv ...string) map[string]string {
		m := map[string]string{"FORGELINE_TOKEN": cliToken}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	tests := []struct {
		name string
		env  map[string]string
		args []string
		code int
		want string
	}{
		{"secret without allow list", with("FORGELINE_GITHUB_WEBHOOK_SECRET", "webhook-secret-0123456789"), nil, 2, "--github-allow"},
		{"allow list without secret", base, []string{"--github-allow", "a/b:main"}, 1, "webhook secret"},
		{"allow list without token", with("FORGELINE_GITHUB_WEBHOOK_SECRET", "webhook-secret-0123456789"), []string{"--github-allow", "a/b:main"}, 1, "token is required"},
		{"malformed allow value", base, []string{"--github-allow", "a/b"}, 2, "owner/repo:branch"},
		{"bad repo name", with("FORGELINE_GITHUB_WEBHOOK_SECRET", "webhook-secret-0123456789", "FORGELINE_GITHUB_TOKEN", "t"), []string{"--github-allow", "nope:main"}, 1, "owner/name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, errs := run(tt.env, tt.args...)
			if code != tt.code || !strings.Contains(errs, tt.want) {
				t.Errorf("exit %d, stderr %q; want exit %d mentioning %q", code, errs, tt.code, tt.want)
			}
		})
	}
}

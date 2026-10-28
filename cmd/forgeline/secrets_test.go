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

	"github.com/cstripathy/forgeline/internal/executor"
)

const secretValue = "s3cret-token-value"

const secretPipeline = `version: v1alpha1
name: needs-secret
steps:
  - id: deploy
    image: alpine
    run: echo
    permissions:
      secrets: [TOKEN]
  - id: plain
    image: alpine
    run: echo
`

func writePipeline(t *testing.T, body string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "pipeline.yaml")
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func setEnviron(t *testing.T, kv ...string) {
	t.Helper()
	old := environ
	environ = func() []string { return append([]string{"PATH=/usr/bin"}, kv...) }
	t.Cleanup(func() { environ = old })
}

// leaky prints the secret it was given and records who received secrets.
func leaky(got map[string]map[string]string) *executor.Fake {
	var mu sync.Mutex // steps in one layer run in parallel
	return &executor.Fake{Handler: func(_ context.Context, s executor.StepRun, logs io.Writer) (executor.Result, error) {
		mu.Lock()
		got[s.ID] = s.Secrets
		mu.Unlock()
		for _, v := range s.Secrets {
			io.WriteString(logs, "the token is "+v+"\n")
		}
		return executor.Result{}, nil
	}}
}

func TestRunGivesSecretsToStepsThatListThemAndMasksLogs(t *testing.T) {
	setEnviron(t, "FORGELINE_SECRET_TOKEN="+secretValue)
	got := map[string]map[string]string{}
	var out, errb bytes.Buffer
	code := runPipeline([]string{"--no-cache", writePipeline(t, secretPipeline)}, &out, &errb, leaky(got))
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), errb.String())
	}
	if got["deploy"]["TOKEN"] != secretValue {
		t.Errorf("deploy received %v", got["deploy"])
	}
	if len(got["plain"]) != 0 {
		t.Errorf("plain received %v", got["plain"])
	}
	if strings.Contains(out.String()+errb.String(), secretValue) {
		t.Fatalf("the secret is in the output:\n%s%s", out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "[deploy] the token is ***") {
		t.Errorf("want a masked line in:\n%s", out.String())
	}
}

func TestRunFailsClearlyWhenASecretIsNotConfigured(t *testing.T) {
	setEnviron(t) // none
	got := map[string]map[string]string{}
	var out, errb bytes.Buffer
	code := runPipeline([]string{"--no-cache", writePipeline(t, secretPipeline)}, &out, &errb, leaky(got))
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out.String(), "FORGELINE_SECRET_TOKEN") {
		t.Errorf("the message should say how to set it:\n%s", out.String())
	}
	if _, ran := got["deploy"]; ran {
		t.Error("the step must not start without its secret")
	}
	if _, ran := got["plain"]; !ran {
		t.Error("an unrelated step should still run")
	}
}

func TestRunRejectsBadSecretConfigWithoutEchoingIt(t *testing.T) {
	setEnviron(t, "FORGELINE_SECRET_TOKEN=short")
	var out, errb bytes.Buffer
	code := runPipeline([]string{writePipeline(t, secretPipeline)}, &out, &errb, &executor.Fake{})
	if code != 1 || !strings.Contains(errb.String(), "FORGELINE_SECRET_TOKEN") || strings.Contains(errb.String(), "=short") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}

func TestServerGivesSecretsToSubmittedRunsAndMasksStoredLogs(t *testing.T) {
	setEnviron(t, "FORGELINE_SECRET_TOKEN="+secretValue)
	got := map[string]map[string]string{}
	url := startServer(t, leaky(got), map[string]string{"FORGELINE_TOKEN": cliToken})

	code, out, errs := submit(t, url, cliToken, "--wait", "--no-source", writePipeline(t, secretPipeline))
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	if got["deploy"]["TOKEN"] != secretValue {
		t.Errorf("deploy received %v", got["deploy"])
	}
	if strings.Contains(out+errs, secretValue) {
		t.Fatalf("the secret came back through the API:\n%s", out)
	}
	if !strings.Contains(out, "the token is ***") {
		t.Errorf("want a masked line in:\n%s", out)
	}
}

func TestServerRefusesToStartWithBadSecretConfig(t *testing.T) {
	setEnviron(t, "FORGELINE_SECRET_TOKEN=short")
	var out, errb bytes.Buffer
	code := runServer(context.Background(), []string{"--listen", "127.0.0.1:0", "--data-dir", t.TempDir()},
		&out, &errb, &executor.Fake{}, func(k string) string { return map[string]string{"FORGELINE_TOKEN": cliToken}[k] })
	if code != 1 || !strings.Contains(errb.String(), "FORGELINE_SECRET_TOKEN") || strings.Contains(errb.String(), "=short") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}

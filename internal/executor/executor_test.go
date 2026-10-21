package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDockerArgs(t *testing.T) {
	tests := []struct {
		name string
		run  StepRun
		want []string
	}{
		{"script with defaults", StepRun{ID: "a", Image: "alpine", Script: "echo hi", Workspace: "/ws"},
			[]string{"run", "--rm", "--name", "n", "-w", "/workspace", "-v", "/ws:/workspace",
				"--entrypoint", "sh", "alpine", "-c", "echo hi"}},
		{"command with env and workdir", StepRun{ID: "a", Image: "golang", Command: []string{"go", "test", "./..."},
			Env: map[string]string{"B": "2", "A": "1"}, WorkDir: "/src"},
			[]string{"run", "--rm", "--name", "n", "-w", "/src", "-e", "A=1", "-e", "B=2",
				"--entrypoint", "go", "golang", "test", "./..."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dockerArgs("n", tt.run)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("args = %v\nwant   %v", got, tt.want)
			}
		})
	}
}

func TestDockerArgsRejects(t *testing.T) {
	for name, run := range map[string]StepRun{
		"no image": {ID: "a", Script: "true"},
		"neither":  {ID: "a", Image: "alpine"},
		"both":     {ID: "a", Image: "alpine", Script: "true", Command: []string{"true"}},
	} {
		if _, err := dockerArgs("n", run); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestFakeTimeout(t *testing.T) {
	f := &Fake{Handler: func(ctx context.Context, s StepRun, _ io.Writer) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}}
	_, err := f.Run(context.Background(), StepRun{ID: "slow", Timeout: 10 * time.Millisecond}, io.Discard)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestFakeCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &Fake{Handler: func(ctx context.Context, s StepRun, _ io.Writer) (Result, error) {
		cancel()
		<-ctx.Done()
		return Result{}, ctx.Err()
	}}
	_, err := f.Run(ctx, StepRun{ID: "x", Timeout: time.Minute}, io.Discard)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func dockerOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}
}

func TestDockerRunsRealContainer(t *testing.T) {
	dockerOrSkip(t)
	image := os.Getenv("FORGELINE_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	ws := t.TempDir()
	var logs bytes.Buffer

	res, err := Docker{}.Run(context.Background(),
		StepRun{ID: "t", Image: image, Script: "echo $GREETING > out.txt; cat out.txt; exit 3",
			Env: map[string]string{"GREETING": "hello"}, Workspace: ws, Timeout: time.Minute}, &logs)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || !strings.Contains(logs.String(), "hello") {
		t.Fatalf("exit=%d logs=%q, want exit 3 and hello", res.ExitCode, logs.String())
	}
	if b, _ := os.ReadFile(ws + "/out.txt"); strings.TrimSpace(string(b)) != "hello" {
		t.Fatalf("workspace file = %q, want hello", b)
	}

	_, err = Docker{}.Run(context.Background(),
		StepRun{ID: "slow", Image: image, Script: "sleep 30", Timeout: 2 * time.Second}, io.Discard)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

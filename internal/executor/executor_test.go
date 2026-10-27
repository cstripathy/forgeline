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
			[]string{"run", "--rm", "--name", "n", "--security-opt", "no-new-privileges", "-w", "/workspace", "-v", "/ws:/workspace",
				"--entrypoint", "sh", "alpine", "-c", "echo hi"}},
		{"command with env and workdir", StepRun{ID: "a", Image: "golang", Command: []string{"go", "test", "./..."},
			Env: map[string]string{"B": "2", "A": "1"}, WorkDir: "/src"},
			[]string{"run", "--rm", "--name", "n", "--security-opt", "no-new-privileges", "-w", "/src", "-e", "A=1", "-e", "B=2",
				"--entrypoint", "go", "golang", "test", "./..."}},
		{"network none", StepRun{ID: "a", Image: "alpine", Script: "true", Network: "none"},
			[]string{"run", "--rm", "--name", "n", "--security-opt", "no-new-privileges", "-w", "/workspace", "--network", "none",
				"--entrypoint", "sh", "alpine", "-c", "true"}},
		{"network egress is the default", StepRun{ID: "a", Image: "alpine", Script: "true", Network: "egress"},
			[]string{"run", "--rm", "--name", "n", "--security-opt", "no-new-privileges", "-w", "/workspace",
				"--entrypoint", "sh", "alpine", "-c", "true"}},
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
		"no image":        {ID: "a", Script: "true"},
		"neither":         {ID: "a", Image: "alpine"},
		"both":            {ID: "a", Image: "alpine", Script: "true", Command: []string{"true"}},
		"unknown network": {ID: "a", Image: "alpine", Script: "true", Network: "host"},
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
		image = "alpine:3.24"
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

// TestDockerSandboxProperties tries to break the isolation a step is promised, against a real
// container. See docs/design/005-threat-model.md, threats T1 and T2.
func TestDockerSandboxProperties(t *testing.T) {
	dockerOrSkip(t)
	image := os.Getenv("FORGELINE_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.24"
	}
	run := func(network, script string) (int, string) {
		t.Helper()
		var logs bytes.Buffer
		res, err := Docker{}.Run(context.Background(),
			StepRun{ID: "sandbox", Image: image, Script: script, Network: network, Timeout: time.Minute}, &logs)
		if err != nil {
			t.Fatalf("network %q: %v\n%s", network, err, logs.String())
		}
		return res.ExitCode, logs.String()
	}

	t.Run("network none leaves only loopback", func(t *testing.T) {
		code, out := run("none", `ls /sys/class/net`)
		if code != 0 || strings.TrimSpace(out) != "lo" {
			t.Fatalf("interfaces = %q (exit %d), want only lo", out, code)
		}
	})

	t.Run("network none blocks every way out", func(t *testing.T) {
		// Each attempt must fail. IPs avoid depending on DNS; the last one needs DNS.
		script := `
fail=0
for target in 1.1.1.1:53 8.8.8.8:443 172.17.0.1:22 169.254.169.254:80; do
  if nc -w 2 -z ${target%%:*} ${target##*:} 2>/dev/null; then echo "REACHED $target"; fail=1; fi
done
if nslookup -timeout=2 example.com >/dev/null 2>&1; then echo "RESOLVED example.com"; fail=1; fi
if wget -q -T 2 -O /dev/null http://example.com 2>/dev/null; then echo "FETCHED example.com"; fail=1; fi
echo done; exit $fail`
		code, out := run("none", script)
		if code != 0 {
			t.Fatalf("a network:none step got out (exit %d):\n%s", code, out)
		}
	})

	t.Run("the default keeps a network interface", func(t *testing.T) {
		// A control, so the test above cannot pass because the container has no network anyway.
		for _, network := range []string{"", "egress"} {
			code, out := run(network, `ls /sys/class/net`)
			if code != 0 || !strings.Contains(out, "eth0") {
				t.Errorf("network %q: interfaces = %q, want eth0 present", network, out)
			}
		}
	})

	t.Run("no_new_privs is set for every step", func(t *testing.T) {
		// This checks the setting, not an exploit: the kernel never lets a process clear the
		// flag, and with it set an exec cannot gain privileges through setuid or file
		// capabilities. A test that shows a blocked escalation needs a setuid binary that does
		// not drop privileges itself (busybox does), which this image does not have.
		for _, network := range []string{"", "none"} {
			code, out := run(network, `grep NoNewPrivs /proc/self/status`)
			if code != 0 || strings.TrimSpace(out) != "NoNewPrivs:\t1" {
				t.Errorf("network %q: %q (exit %d), want NoNewPrivs 1", network, out, code)
			}
		}
	})
}

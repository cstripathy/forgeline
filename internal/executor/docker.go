package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// WorkspaceMount is where the workspace appears inside the container and the default workdir.
const WorkspaceMount = "/workspace"

// Docker runs steps with the docker CLI. It needs a reachable Docker daemon.
type Docker struct {
	// Binary is the docker CLI to run. Empty means "docker".
	Binary string
}

// Run runs the step as `docker run --rm` and returns the container's exit code.
func (d Docker) Run(ctx context.Context, s StepRun, logs io.Writer) (Result, error) {
	name, err := containerName()
	if err != nil {
		return Result{}, err
	}
	envFile, cleanup, err := writeSecretsFile(s)
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	args, err := dockerArgs(name, s, envFile)
	if err != nil {
		return Result{}, err
	}
	bin := d.Binary
	if bin == "" {
		bin = "docker"
	}

	runCtx, cancel := withTimeout(ctx, s)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Stdout = logs
	cmd.Stderr = logs
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()

	if cerr := classify(ctx, runCtx, s); cerr != nil {
		// Killing the CLI does not stop the container, so remove it by name.
		rm := exec.Command(bin, "rm", "-f", name)
		_ = rm.Run()
		return Result{}, cerr
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		// Docker itself exits 125 (daemon error) or 126/127 (cannot run command); those
		// come back as exit codes too, with the reason in the log stream.
		return Result{ExitCode: exitErr.ExitCode()}, nil
	}
	if runErr != nil {
		return Result{}, fmt.Errorf("step %q: running %s: %w", s.ID, bin, runErr)
	}
	return Result{}, nil
}

func containerName() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "forgeline-" + hex.EncodeToString(b[:]), nil
}

// secretNameRE is the shape of an environment variable name. The executor checks it again
// because it is the last line of defence before a name becomes a line in a file.
var secretNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// writeSecretsFile writes the step's secrets as NAME=value lines to a file only the current
// user can read, and returns its path and a function that removes it. With no secrets it
// returns an empty path and a no-op.
//
// A file is used instead of `-e NAME=value` so values are not in the docker process's argv,
// and instead of `-e NAME` with the values in the docker process's environment so that no
// step-controlled setting (DOCKER_HOST, DOCKER_CONFIG, proxy variables) can reach the docker
// CLI itself. Errors name the secret, never its value.
func writeSecretsFile(s StepRun) (string, func(), error) {
	if len(s.Secrets) == 0 {
		return "", func() {}, nil
	}
	names := make([]string, 0, len(s.Secrets))
	for n, v := range s.Secrets {
		if !secretNameRE.MatchString(n) {
			return "", nil, fmt.Errorf("step %q: invalid secret name %q", s.ID, n)
		}
		if strings.ContainsAny(v, "\n\r\x00") {
			return "", nil, fmt.Errorf("step %q: secret %q contains a newline or NUL byte, which an env file cannot carry", s.ID, n)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	dir, err := os.MkdirTemp("", "forgeline-secrets-") // mode 0700
	if err != nil {
		return "", nil, fmt.Errorf("step %q: preparing secrets: %w", s.ID, err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n + "=" + s.Secrets[n] + "\n")
	}
	path := filepath.Join(dir, "env")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("step %q: preparing secrets: %w", s.ID, err)
	}
	return path, cleanup, nil
}

// dockerArgs builds the arguments after `docker`. It is separate from Run so it can be tested
// without Docker. envFile is the path from writeSecretsFile, or "".
func dockerArgs(name string, s StepRun, envFile string) ([]string, error) {
	if s.Image == "" {
		return nil, fmt.Errorf("step %q: image is required", s.ID)
	}
	if (s.Script == "") == (len(s.Command) == 0) {
		return nil, fmt.Errorf("step %q: set exactly one of script or command", s.ID)
	}
	workdir := s.WorkDir
	if workdir == "" {
		workdir = WorkspaceMount
	}
	// no-new-privileges stops a process gaining privileges through setuid binaries, whatever
	// the image does. It is on for every step; see docs/design/005-threat-model.md (T2).
	args := []string{"run", "--rm", "--name", name, "--security-opt", "no-new-privileges", "-w", workdir}
	switch s.Network {
	case "", "egress":
	case "none":
		args = append(args, "--network", "none")
	default:
		return nil, fmt.Errorf("step %q: unknown network %q", s.ID, s.Network)
	}
	if s.Workspace != "" {
		abs, err := filepath.Abs(s.Workspace)
		if err != nil {
			return nil, fmt.Errorf("step %q: workspace: %w", s.ID, err)
		}
		args = append(args, "-v", abs+":"+WorkspaceMount)
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable argument order
	for _, k := range keys {
		args = append(args, "-e", k+"="+s.Env[k])
	}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	if s.Script != "" {
		args = append(args, "--entrypoint", "sh", s.Image, "-c", s.Script)
	} else {
		args = append(args, "--entrypoint", s.Command[0], s.Image)
		args = append(args, s.Command[1:]...)
	}
	return args, nil
}

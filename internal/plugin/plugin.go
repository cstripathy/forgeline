// Package plugin runs third-party MCP servers shipped as container images (design note 018). Every
// call starts a fresh container and speaks MCP over its standard input and output. What the
// container may do is decided here, by a fixed command line, never by the plugin's own manifest.
package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/forgelinehq/forgeline/internal/mcp"
)

var (
	// A pinned image is name@sha256:digest, or a local image's id (sha256:...), which is a content hash too.
	imageRE = regexp.MustCompile(`^([a-z0-9][a-z0-9._/-]{0,200}@)?sha256:[0-9a-f]{64}$`)
	envRE   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	idRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}$`)
)

// Spec is what an administrator approved for one plugin.
type Spec struct {
	ID      string
	Image   string            // name@sha256:digest or sha256:id; a tag alone is refused
	Network bool              // false is --network none
	Env     map[string]string // granted secrets, passed for one call and never put on a command line
}

// Limits bound one call.
type Limits struct {
	Memory    string // docker --memory, e.g. "256m"
	CPUs      string // docker --cpus
	Pids      int
	Timeout   time.Duration
	MaxOutput int // bytes of tool text kept
}

// DefaultLimits are used when a Runner has none.
var DefaultLimits = Limits{Memory: "256m", CPUs: "0.5", Pids: 64, Timeout: 30 * time.Second, MaxOutput: 32 << 10}

// Check refuses a spec that could not be run safely.
func (s Spec) Check() error {
	if !idRE.MatchString(s.ID) {
		return fmt.Errorf("plugin id %q is not a lowercase slug", s.ID)
	}
	if !imageRE.MatchString(s.Image) {
		return errors.New("the image must be pinned by digest (name@sha256:...)")
	}
	for k := range s.Env {
		if !envRE.MatchString(k) {
			return fmt.Errorf("%q is not a valid environment variable name", k)
		}
	}
	return nil
}

// Argv is the whole docker command line for one call. Secret values are not in it: `-e NAME` makes
// docker read the value from its own environment, which keeps them out of the process list.
func Argv(s Spec, l Limits, name string) []string {
	a := []string{"run", "--rm", "-i", "--name", name,
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--user", "65534:65534", "--init",
		"--pids-limit", fmt.Sprint(l.Pids), "--memory", l.Memory, "--cpus", l.CPUs,
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=16m", "--pull", "never"}
	if s.Network {
		a = append(a, "--network", "bridge")
	} else {
		a = append(a, "--network", "none")
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a = append(a, "-e", k)
	}
	return append(a, s.Image)
}

// Runner starts plugin containers through a docker binary.
type Runner struct {
	Docker string // path or name of the docker CLI
	Limits Limits
	// ExtraEnv is added to the docker client's environment (settings it needs to reach its daemon).
	ExtraEnv []string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (r *Runner) limits() Limits {
	l := r.Limits
	d := DefaultLimits
	if l.Memory == "" {
		l.Memory = d.Memory
	}
	if l.CPUs == "" {
		l.CPUs = d.CPUs
	}
	if l.Pids == 0 {
		l.Pids = d.Pids
	}
	if l.Timeout == 0 {
		l.Timeout = d.Timeout
	}
	if l.MaxOutput == 0 {
		l.MaxOutput = d.MaxOutput
	}
	return l
}

// lock makes calls to one plugin go one at a time.
func (r *Runner) lock(id string) func() {
	r.mu.Lock()
	if r.locks == nil {
		r.locks = map[string]*sync.Mutex{}
	}
	m := r.locks[id]
	if m == nil {
		m = &sync.Mutex{}
		r.locks[id] = m
	}
	r.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// session starts the container, runs fn against it and always removes the container.
func (r *Runner) session(ctx context.Context, s Spec, fn func(context.Context, *mcp.Client, Limits) error) error {
	if err := s.Check(); err != nil {
		return err
	}
	l := r.limits()
	defer r.lock(s.ID)()
	ctx, cancel := context.WithTimeout(ctx, l.Timeout)
	defer cancel()
	b := make([]byte, 6)
	rand.Read(b)
	name := "forgeline-plugin-" + hex.EncodeToString(b)
	cmd := exec.Command(r.Docker, Argv(s, l, name)...)
	cmd.Env = append(cleanEnv(s.Env), r.ExtraEnv...)
	c, err := mcp.Start(ctx, cmd)
	if err != nil {
		r.kill(name)
		return fmt.Errorf("the plugin did not start: %w", err)
	}
	defer func() { c.Close(); r.kill(name) }()
	return fn(ctx, c, l)
}

// kill removes a container that may still be running after a timeout. Failing is fine: --rm
// handles the normal exit.
func (r *Runner) kill(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Docker, "rm", "-f", name)
	cmd.Env = append(cleanEnv(nil), r.ExtraEnv...)
	cmd.Run()
}

// cleanEnv gives the docker client only what it needs to reach the daemon, plus the granted secrets.
func cleanEnv(grants map[string]string) []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT", "XDG_RUNTIME_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range grants {
		env = append(env, k+"="+v)
	}
	return env
}

// Tools starts the plugin and lists what it offers, so an install can check the manifest.
func (r *Runner) Tools(ctx context.Context, s Spec) ([]mcp.Tool, error) {
	var out []mcp.Tool
	err := r.session(ctx, s, func(ctx context.Context, c *mcp.Client, _ Limits) (err error) {
		out, err = c.Tools(ctx)
		return
	})
	return out, err
}

// Call runs one tool. The text is cut at MaxOutput; a tool that reports an error is an error.
func (r *Runner) Call(ctx context.Context, s Spec, tool string, args map[string]any) (string, error) {
	var text string
	err := r.session(ctx, s, func(ctx context.Context, c *mcp.Client, l Limits) error {
		res, err := c.Call(ctx, tool, args)
		if err != nil {
			return err
		}
		text = res.Text
		if len(text) > l.MaxOutput {
			text = strings.ToValidUTF8(text[:l.MaxOutput], "") + "\n[output cut]"
		}
		if res.IsError {
			return fmt.Errorf("the plugin reported an error: %s", text)
		}
		return nil
	})
	return text, err
}

// Ping checks that the docker daemon answers.
func (r *Runner) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Docker, "version", "--format", "{{.Server.Version}}")
	cmd.Env = append(cleanEnv(nil), r.ExtraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker did not answer: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Ensure makes sure the image is present, pulling it if it is not. A pull by digest cannot return
// different bytes, so what runs is what was pinned.
func (r *Runner) Ensure(ctx context.Context, image string) error {
	run := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, r.Docker, args...)
		cmd.Env = append(cleanEnv(nil), r.ExtraEnv...)
		return cmd.CombinedOutput()
	}
	if err := (Spec{ID: "check", Image: image}).Check(); err != nil {
		return err
	}
	if _, err := run("image", "inspect", "--format", "{{.Id}}", image); err == nil {
		return nil
	}
	if !strings.Contains(image, "@") {
		return errors.New("the image is not on this host and an image id cannot be pulled")
	}
	if out, err := run("pull", "--quiet", image); err != nil {
		return fmt.Errorf("could not pull the image: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

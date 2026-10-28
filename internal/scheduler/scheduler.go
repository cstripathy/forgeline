// Package scheduler runs a planned pipeline graph through an Executor.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/cstripathy/forgeline/internal/executor"
	"github.com/cstripathy/forgeline/internal/pipeline"
	"github.com/cstripathy/forgeline/internal/redact"
	"github.com/cstripathy/forgeline/internal/secrets"
)

// Status is the outcome of one step.
type Status string

const (
	StatusSucceeded Status = "succeeded"
	StatusCached    Status = "cached"   // not run: an earlier success with the same cache key exists
	StatusFailed    Status = "failed"   // exited non-zero, timed out, or could not run
	StatusSkipped   Status = "skipped"  // a step it needs did not succeed
	StatusCanceled  Status = "canceled" // the context ended before or while it ran
)

// ok reports whether the step counts as a success for its dependents.
func (s Status) ok() bool { return s == StatusSucceeded || s == StatusCached }

// StepResult is what happened to one step.
type StepResult struct {
	ID       string
	Status   Status
	ExitCode int
	// Err explains a failed or canceled step; for a skipped step it names the blocking step.
	Err      error
	Duration time.Duration
}

// Report is the full outcome of a run, in layer order then declaration order.
type Report struct {
	Steps []StepResult
}

// OK reports whether every step succeeded.
func (r *Report) OK() bool {
	for _, s := range r.Steps {
		if !s.Status.ok() {
			return false
		}
	}
	return true
}

// Options configures a run.
type Options struct {
	// Parallelism bounds how many steps run at once. Zero or less means 1.
	Parallelism int
	// Workspace is the host directory mounted into every step.
	Workspace string
	// Logs returns the writer for a step's output. Nil discards output. Each step gets its
	// own writer, but steps run concurrently, so a shared underlying writer must be safe
	// for concurrent use.
	Logs func(stepID string) io.Writer
	// Secrets are the values steps may request through permissions.secrets, by name. A step
	// receives only the ones it lists. Every value here is masked in every step's output,
	// including the ones that step did not request.
	Secrets map[string]string
	// SecretsDenied, if not empty, refuses every step that requests a secret and says why.
	// The run still executes steps that request none. Use it for runs that must not hold
	// secrets, such as ones started by untrusted input.
	SecretsDenied string
	// OnStepStart, if set, is called just before a step is handed to the executor.
	OnStepStart func(stepID string)
	// OnStepDone, if set, is called once for every step with its final result, including
	// skipped and canceled steps. Both callbacks run on scheduler goroutines, possibly
	// concurrently, so they must be safe for concurrent use.
	OnStepDone func(StepResult)
}

// Run executes the graph layer by layer. When a step fails, every step that needs it,
// directly or indirectly, is skipped; unrelated steps still run to completion. Cancelling
// ctx stops running steps and marks everything not yet started as canceled.
func Run(ctx context.Context, g *pipeline.Graph, ex executor.Executor, opts Options) *Report {
	limit := opts.Parallelism
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	status := make(map[string]Status)
	report := &Report{}

	for _, layer := range g.Layers {
		results := make([]StepResult, len(layer))
		var wg sync.WaitGroup

		for i, step := range layer {
			finish := func(r StepResult) {
				results[i] = r
				if opts.OnStepDone != nil {
					opts.OnStepDone(r)
				}
			}
			if blocker := blockedBy(step, status); blocker != "" {
				finish(StepResult{ID: step.ID, Status: StatusSkipped,
					Err: fmt.Errorf("needs %q, which did not succeed", blocker)})
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					finish(StepResult{ID: step.ID, Status: StatusCanceled, Err: ctx.Err()})
					return
				}
				finish(runStep(ctx, step, ex, opts))
			}()
		}
		wg.Wait()

		for _, r := range results {
			status[r.ID] = r.Status
			report.Steps = append(report.Steps, r)
		}
	}
	return report
}

// blockedBy returns the first needed step that did not succeed, or "".
func blockedBy(s *pipeline.Step, status map[string]Status) string {
	for _, need := range s.Needs {
		if !status[need].ok() {
			return need
		}
	}
	return ""
}

func runStep(ctx context.Context, s *pipeline.Step, ex executor.Executor, opts Options) StepResult {
	if err := ctx.Err(); err != nil {
		return StepResult{ID: s.ID, Status: StatusCanceled, Err: err}
	}
	run, err := toStepRun(s, opts.Workspace)
	if err != nil {
		return StepResult{ID: s.ID, Status: StatusFailed, Err: err}
	}
	if run.Secrets, err = grantSecrets(s, opts); err != nil {
		return StepResult{ID: s.ID, Status: StatusFailed, Err: err}
	}
	var logs io.Writer = io.Discard
	if opts.Logs != nil {
		logs = opts.Logs(s.ID)
	}
	if len(opts.Secrets) > 0 {
		masked := redact.New(logs, secrets.Values(opts.Secrets))
		logs = masked
		defer masked.Close() // releases output held back in case it was the start of a value
	}

	if opts.OnStepStart != nil {
		opts.OnStepStart(s.ID)
	}
	start := time.Now()
	res, err := ex.Run(ctx, run, logs)
	out := StepResult{ID: s.ID, ExitCode: res.ExitCode, Duration: time.Since(start)}
	switch {
	case ctx.Err() != nil && err != nil:
		out.Status, out.Err = StatusCanceled, ctx.Err()
	case err != nil:
		out.Status, out.Err = StatusFailed, err
	case res.ExitCode != 0:
		out.Status, out.Err = StatusFailed, fmt.Errorf("exited with code %d", res.ExitCode)
	case res.Cached:
		out.Status = StatusCached
	default:
		out.Status = StatusSucceeded
	}
	return out
}

// grantSecrets returns the secrets the step asked for, or an error that names what is wrong.
// Errors name secrets, never values.
func grantSecrets(s *pipeline.Step, opts Options) (map[string]string, error) {
	names := s.Permissions.Secrets
	if len(names) == 0 {
		return nil, nil
	}
	if opts.SecretsDenied != "" {
		return nil, fmt.Errorf("step requests secrets (%s) but %s", strings.Join(names, ", "), opts.SecretsDenied)
	}
	granted := make(map[string]string, len(names))
	for _, n := range names {
		v, ok := opts.Secrets[n]
		if !ok {
			return nil, fmt.Errorf("secret %q is not configured; set %s%s in the environment of the process that runs steps", n, secrets.Prefix, n)
		}
		granted[n] = v
	}
	return granted, nil
}

func toStepRun(s *pipeline.Step, workspace string) (executor.StepRun, error) {
	run := executor.StepRun{
		ID: s.ID, Image: s.Image, Script: s.Run, Command: s.Command,
		Env: s.Env, WorkDir: s.WorkDir, Workspace: workspace, Inputs: s.Inputs,
		Network: s.Permissions.Network,
	}
	if s.Agent != nil {
		run.Command = s.Agent.Argv()
	}
	if s.Timeout != "" {
		d, err := time.ParseDuration(s.Timeout)
		if err != nil {
			return run, fmt.Errorf("timeout: %w", err)
		}
		run.Timeout = d
	}
	return run, nil
}

// Err returns nil when every step succeeded, else an error wrapping ErrFailed that lists the
// steps that did not succeed.
func (r *Report) Err() error {
	var bad []string
	for _, s := range r.Steps {
		if !s.Status.ok() {
			bad = append(bad, fmt.Sprintf("%s (%s)", s.ID, s.Status))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrFailed, strings.Join(bad, ", "))
}

// ErrFailed is wrapped by Report.Err.
var ErrFailed = errors.New("pipeline did not succeed")

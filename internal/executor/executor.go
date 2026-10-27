// Package executor runs a single pipeline step. The Executor interface is the boundary
// between the scheduler and a container runtime, so the scheduler can be tested without one.
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrTimeout is wrapped by the error an Executor returns when a step exceeds its timeout.
var ErrTimeout = errors.New("step timed out")

// StepRun is everything an Executor needs to run one step.
type StepRun struct {
	ID    string
	Image string
	// Script is a shell script run with `sh -c`. Set this or Command, not both.
	Script string
	// Command is a program and arguments run without a shell.
	Command []string
	Env     map[string]string
	// Secrets are environment variables whose values must not appear in argv, logs or the
	// cache key. Real executors pass them through a private file, never `-e NAME=value`.
	// The scheduler fills this with only the secrets the step lists.
	Secrets map[string]string
	// WorkDir is the working directory inside the container. Empty means /workspace.
	WorkDir string
	// Timeout is the maximum run time. Zero means no limit.
	Timeout time.Duration
	// Workspace is the host directory mounted at /workspace.
	Workspace string
	// Network is "none" to run the step with no network at all. Empty or "egress" leaves the
	// container's default network in place.
	Network string
	// Inputs are workspace-relative globs that make up the step's cache key. Real executors
	// ignore them; the cache wrapper reads them.
	Inputs []string
}

// Result describes a step that ran to completion. A non-zero ExitCode is a failed step,
// not an error.
type Result struct {
	ExitCode int
	// Cached is set by the cache wrapper when the step was not run because an earlier
	// success with the same cache key exists.
	Cached bool
}

// Executor runs one step and streams its combined stdout and stderr to logs.
//
// Run returns a non-nil error when the step could not run to completion: it could not be
// started, the context was cancelled, or Timeout elapsed (the error then wraps ErrTimeout).
// A step that runs and exits non-zero returns Result{ExitCode: n} and a nil error.
// Implementations must enforce StepRun.Timeout and stop the step when ctx is cancelled.
type Executor interface {
	Run(ctx context.Context, s StepRun, logs io.Writer) (Result, error)
}

// withTimeout derives the context a run uses: ctx limited by s.Timeout when it is set.
func withTimeout(ctx context.Context, s StepRun) (context.Context, context.CancelFunc) {
	if s.Timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, s.Timeout)
}

// classify turns a finished run's context state into the error Run should return.
// parent is the caller's context, run is the one derived with the timeout.
func classify(parent, run context.Context, s StepRun) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if run.Err() != nil {
		return fmt.Errorf("step %q: %w after %s", s.ID, ErrTimeout, s.Timeout)
	}
	return nil
}

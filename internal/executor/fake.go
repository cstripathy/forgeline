package executor

import (
	"context"
	"io"
	"sync"
)

// Fake is an Executor for tests. Handler decides each step's outcome; with no Handler every
// step succeeds. Fake enforces Timeout like a real executor: Handler's context ends when the
// timeout elapses, and Run then returns an error wrapping ErrTimeout.
type Fake struct {
	Handler func(ctx context.Context, s StepRun, logs io.Writer) (Result, error)

	mu    sync.Mutex
	calls []StepRun
}

// Run records the call and invokes Handler.
func (f *Fake) Run(ctx context.Context, s StepRun, logs io.Writer) (Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()

	if f.Handler == nil {
		return Result{}, nil
	}
	runCtx, cancel := withTimeout(ctx, s)
	defer cancel()
	res, err := f.Handler(runCtx, s, logs)
	if cerr := classify(ctx, runCtx, s); cerr != nil {
		return Result{}, cerr
	}
	return res, err
}

// Calls returns the steps run so far, in the order they started.
func (f *Fake) Calls() []StepRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]StepRun(nil), f.calls...)
}

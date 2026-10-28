package server

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/forgelinehq/forgeline/internal/pipeline"
	"github.com/forgelinehq/forgeline/internal/scheduler"
	"github.com/forgelinehq/forgeline/internal/store"
)

// storeTimeout bounds each bookkeeping write so a stuck database cannot hang a run forever.
const storeTimeout = 15 * time.Second

type runCtl struct {
	cancel    context.CancelFunc
	requested bool // a user asked to cancel
}

func (s *Server) workspace(runID string) string {
	return filepath.Join(s.cfg.DataDir, "runs", runID)
}

func (s *Server) worker(ctx context.Context) {
	for ctx.Err() == nil {
		run, ok, err := s.cfg.Store.ClaimQueuedRun(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			s.log.Error("claiming a run", "err", err)
		case ok:
			s.execute(ctx, run)
			continue
		}
		select {
		case <-s.wake:
		case <-time.After(time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// execute runs one claimed run to a final state and records everything along the way.
func (s *Server) execute(ctx context.Context, run store.Run) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctl := s.register(run.ID, cancel)
	defer s.unregister(run.ID)

	// Bookkeeping must survive cancellation, or a canceled run would never record its end.
	bg := context.WithoutCancel(ctx)
	withTimeout := func() (context.Context, context.CancelFunc) { return context.WithTimeout(bg, storeTimeout) }
	finishRun := func(status store.RunStatus, reason string) {
		c, done := withTimeout()
		defer done()
		if _, err := s.cfg.Store.UpdateRun(c, run.ID, store.RunUpdate{Status: status, Reason: reason}); err != nil {
			s.log.Error("recording run result", "run", run.ID, "err", err)
			return
		}
		s.reportFinished(run, status, reason)
	}

	p, err := pipeline.Load(strings.NewReader(run.PipelineYAML))
	if err == nil {
		var g *pipeline.Graph
		if g, err = pipeline.Plan(p); err == nil {
			s.runGraph(runCtx, run, g, ctl, bg, finishRun)
			return
		}
	}
	finishRun(store.RunFailed, fmt.Sprintf("stored pipeline is invalid: %v", err))
}

func (s *Server) runGraph(runCtx context.Context, run store.Run, g *pipeline.Graph, ctl *runCtl,
	bg context.Context, finishRun func(store.RunStatus, string)) {

	update := func(stepID string, u store.StepUpdate) {
		c, done := context.WithTimeout(bg, storeTimeout)
		defer done()
		if _, err := s.cfg.Store.UpdateStep(c, run.ID, stepID, u); err != nil {
			s.log.Error("recording step state", "run", run.ID, "step", stepID, "err", err)
		}
	}
	secretsFor, denied := s.secretsFor(bg, run)
	report := scheduler.Run(runCtx, g, s.cfg.Executor, scheduler.Options{
		Parallelism:    s.cfg.Parallelism,
		Workspace:      s.workspace(run.ID),
		Secrets:        secretsFor,
		SecretsDenied:  denied,
		Workflow:       s.workflowStep(run),
		WorkflowDenied: workflowDeniedFor(run),
		Logs: func(stepID string) io.Writer {
			return &logWriter{log: s.log, store: s.cfg.Store, ctx: bg, runID: run.ID, stepID: stepID, limit: s.cfg.MaxLogBytes}
		},
		OnStepStart: func(id string) { update(id, store.StepUpdate{Status: store.StepRunning}) },
		OnStepDone: func(r scheduler.StepResult) {
			u := store.StepUpdate{ExitCode: r.ExitCode}
			if r.Err != nil {
				u.Error = r.Err.Error()
			}
			switch r.Status {
			case scheduler.StatusSucceeded:
				u.Status = store.StepSucceeded
			case scheduler.StatusCached:
				u.Status = store.StepCached
			case scheduler.StatusSkipped:
				u.Status = store.StepSkipped
			case scheduler.StatusCanceled:
				u.Status = store.StepCanceled
			default:
				u.Status = store.StepFailed
			}
			update(r.ID, u)
		},
	})

	s.mu.Lock()
	userCanceled := ctl.requested
	s.mu.Unlock()
	switch {
	case report.OK():
		finishRun(store.RunSucceeded, "")
	case userCanceled:
		finishRun(store.RunCanceled, "canceled by user")
	case bg.Err() == nil && runCtx.Err() != nil:
		// The worker context ended, not the run's: the server is shutting down.
		finishRun(store.RunCanceled, "server shutting down")
	default:
		finishRun(store.RunFailed, report.Err().Error())
	}
}

// secretsFor decides what secrets a run may use. A run started by a webhook is built from
// content a pull request or a push controls, so until runs have a trust level (note 005,
// slice 3) it gets none: a step that asks for one fails with this reason instead of running.
func (s *Server) secretsFor(ctx context.Context, run store.Run) (map[string]string, string) {
	if run.Trigger == store.TriggerGitHub {
		return nil, "runs started by GitHub receive no secrets yet (docs/design/005-threat-model.md, slice 3)"
	}
	return s.stepSecrets(ctx), ""
}

func (s *Server) register(id string, cancel context.CancelFunc) *runCtl {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctl := &runCtl{cancel: cancel}
	s.ctls[id] = ctl
	if s.pendingCancel[id] {
		delete(s.pendingCancel, id)
		ctl.requested = true
		cancel()
	}
	return ctl
}

func (s *Server) unregister(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ctls, id)
	delete(s.pendingCancel, id)
}

// requestCancel asks a running run to stop. If the worker has not registered the run yet, the
// request is remembered and applied at registration.
func (s *Server) requestCancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctl, ok := s.ctls[id]; ok {
		ctl.requested = true
		ctl.cancel()
		return
	}
	s.pendingCancel[id] = true
}

// logWriter stores a step's output as log chunks, up to a size limit. Store errors are
// logged and swallowed: failing the write would fail the step over a bookkeeping problem.
type logWriter struct {
	log     interface{ Error(string, ...any) }
	store   store.Store
	ctx     context.Context
	runID   string
	stepID  string
	limit   int64
	written int64
	done    bool // limit reached, marker written
}

func (w *logWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.done || n == 0 {
		return n, nil
	}
	if remaining := w.limit - w.written; int64(len(p)) > remaining {
		p = append(append([]byte(nil), p[:remaining]...),
			fmt.Sprintf("\n[log truncated: step output exceeded %d bytes]\n", w.limit)...)
		w.done = true
	}
	w.written += int64(len(p))
	c, cancel := context.WithTimeout(w.ctx, storeTimeout)
	defer cancel()
	if _, err := w.store.AppendLog(c, w.runID, w.stepID, p); err != nil {
		w.log.Error("storing log chunk", "run", w.runID, "step", w.stepID, "err", err)
	}
	return n, nil
}

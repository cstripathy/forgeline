package store

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Memory is an in-memory Store for tests. It is not durable.
type Memory struct {
	mu    sync.Mutex
	runs  map[string]*memRun
	order []string // run IDs, oldest first
}

type memRun struct {
	run   Run
	steps []*Step
	logs  map[string][]LogChunk
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{runs: map[string]*memRun{}} }

func (m *Memory) Close() {}

func now() time.Time { return time.Now().UTC() }

func (m *Memory) CreateRun(_ context.Context, r Run, stepIDs []string) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.ID = newID()
	r.Status = RunQueued
	r.CreatedAt = now()
	r.StartedAt, r.FinishedAt = nil, nil
	mr := &memRun{run: r, logs: map[string][]LogChunk{}}
	for _, id := range stepIDs {
		mr.steps = append(mr.steps, &Step{RunID: r.ID, StepID: id, Status: StepPending})
	}
	m.runs[r.ID] = mr
	m.order = append(m.order, r.ID)
	return r, nil
}

func (m *Memory) GetRun(_ context.Context, id string) (Run, []Step, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mr, ok := m.runs[id]
	if !ok {
		return Run{}, nil, ErrNotFound
	}
	return copyRun(mr.run), copySteps(mr.steps), nil
}

func (m *Memory) ListRuns(_ context.Context, opts ListOptions) ([]Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	end := len(m.order)
	if opts.Before != "" {
		end = -1
		for i, id := range m.order {
			if id == opts.Before {
				end = i
			}
		}
		if end < 0 {
			return nil, fmt.Errorf("before %q: %w", opts.Before, ErrNotFound)
		}
	}
	var out []Run
	for i := end - 1; i >= 0 && len(out) < opts.limit(); i-- {
		out = append(out, copyRun(m.runs[m.order[i]].run))
	}
	return out, nil
}

func (m *Memory) ClaimQueuedRun(_ context.Context) (Run, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.order {
		mr := m.runs[id]
		if mr.run.Status == RunQueued {
			t := now()
			mr.run.Status, mr.run.StartedAt = RunRunning, &t
			return copyRun(mr.run), true, nil
		}
	}
	return Run{}, false, nil
}

func (m *Memory) UpdateRun(_ context.Context, id string, u RunUpdate) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mr, ok := m.runs[id]
	if !ok {
		return Run{}, ErrNotFound
	}
	if !mr.run.Status.canMoveTo(u.Status) {
		return Run{}, fmt.Errorf("run %s: %s -> %s: %w", id, mr.run.Status, u.Status, ErrInvalidTransition)
	}
	t := now()
	if u.Status == RunRunning {
		mr.run.StartedAt = &t
	}
	if u.Status.Terminal() {
		mr.run.FinishedAt = &t
	}
	mr.run.Status = u.Status
	if u.Reason != "" {
		mr.run.Reason = u.Reason
	}
	return copyRun(mr.run), nil
}

func (m *Memory) step(runID, stepID string) (*memRun, *Step, error) {
	mr, ok := m.runs[runID]
	if !ok {
		return nil, nil, ErrNotFound
	}
	for _, s := range mr.steps {
		if s.StepID == stepID {
			return mr, s, nil
		}
	}
	return nil, nil, ErrNotFound
}

func (m *Memory) UpdateStep(_ context.Context, runID, stepID string, u StepUpdate) (Step, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, s, err := m.step(runID, stepID)
	if err != nil {
		return Step{}, err
	}
	if !s.Status.canMoveTo(u.Status) {
		return Step{}, fmt.Errorf("step %s/%s: %s -> %s: %w", runID, stepID, s.Status, u.Status, ErrInvalidTransition)
	}
	t := now()
	if u.Status == StepRunning {
		s.StartedAt = &t
	}
	if u.Status.Terminal() {
		s.FinishedAt = &t
	}
	s.Status, s.ExitCode, s.Error = u.Status, u.ExitCode, u.Error
	return copyStep(*s), nil
}

func (m *Memory) AppendLog(_ context.Context, runID, stepID string, data []byte) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mr, _, err := m.step(runID, stepID)
	if err != nil {
		return 0, err
	}
	chunks := mr.logs[stepID]
	seq := int64(len(chunks)) + 1
	mr.logs[stepID] = append(chunks, LogChunk{RunID: runID, StepID: stepID, Seq: seq,
		Data: append([]byte(nil), data...), CreatedAt: now()})
	return seq, nil
}

func (m *Memory) ReadLog(_ context.Context, runID, stepID string, afterSeq int64, limit int) ([]LogChunk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mr, _, err := m.step(runID, stepID)
	if err != nil {
		return nil, err
	}
	var out []LogChunk
	for _, c := range mr.logs[stepID] {
		if c.Seq <= afterSeq {
			continue
		}
		if limit > 0 && len(out) >= limit {
			break
		}
		c.Data = append([]byte(nil), c.Data...)
		out = append(out, c)
	}
	return out, nil
}

func (m *Memory) FailInterruptedRuns(_ context.Context, reason string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, id := range m.order {
		mr := m.runs[id]
		if mr.run.Status != RunRunning {
			continue
		}
		t := now()
		mr.run.Status, mr.run.Reason, mr.run.FinishedAt = RunFailed, reason, &t
		for _, s := range mr.steps {
			if !s.Status.Terminal() {
				s.Status, s.FinishedAt = StepCanceled, &t
			}
		}
		n++
	}
	return n, nil
}

func copyRun(r Run) Run {
	if r.StartedAt != nil {
		t := *r.StartedAt
		r.StartedAt = &t
	}
	if r.FinishedAt != nil {
		t := *r.FinishedAt
		r.FinishedAt = &t
	}
	return r
}

func copyStep(s Step) Step {
	if s.StartedAt != nil {
		t := *s.StartedAt
		s.StartedAt = &t
	}
	if s.FinishedAt != nil {
		t := *s.FinishedAt
		s.FinishedAt = &t
	}
	return s
}

func copySteps(in []*Step) []Step {
	out := make([]Step, len(in))
	for i, s := range in {
		out[i] = copyStep(*s)
	}
	return out
}

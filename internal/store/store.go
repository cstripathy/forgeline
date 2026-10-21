// Package store persists runs, step results and log chunks. Two implementations share one
// test suite: Memory for tests and Postgres for the server.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNotFound is returned when a run or step does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalidTransition is returned when a status change is not allowed, such as moving a
	// finished run back to running.
	ErrInvalidTransition = errors.New("invalid status transition")
)

// RunStatus is the state of a run.
type RunStatus string

const (
	RunQueued    RunStatus = "queued"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCanceled  RunStatus = "canceled"
)

// Terminal reports whether no further transition is allowed.
func (s RunStatus) Terminal() bool {
	return s == RunSucceeded || s == RunFailed || s == RunCanceled
}

func (s RunStatus) canMoveTo(to RunStatus) bool {
	switch s {
	case RunQueued:
		return to == RunRunning || to == RunCanceled
	case RunRunning:
		return to.Terminal()
	}
	return false
}

// StepStatus is the state of one step in a run.
type StepStatus string

const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepSucceeded StepStatus = "succeeded"
	StepCached    StepStatus = "cached"
	StepFailed    StepStatus = "failed"
	StepSkipped   StepStatus = "skipped"
	StepCanceled  StepStatus = "canceled"
)

// Terminal reports whether no further transition is allowed.
func (s StepStatus) Terminal() bool { return s != StepPending && s != StepRunning }

func (s StepStatus) canMoveTo(to StepStatus) bool {
	switch s {
	case StepPending:
		return to != StepPending
	case StepRunning:
		return to.Terminal()
	}
	return false
}

// Trigger says how a run was started.
type Trigger string

const (
	TriggerAPI    Trigger = "api"
	TriggerGitHub Trigger = "github"
)

// Run is one execution of a pipeline. PipelineYAML is the exact text that ran.
type Run struct {
	ID           string
	PipelineName string
	PipelineYAML string
	Status       RunStatus
	Reason       string
	SourceRepo   string
	SourceRef    string
	SourceSHA    string
	Trigger      Trigger
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

// Step is the state of one step in a run.
type Step struct {
	RunID      string
	StepID     string
	Status     StepStatus
	ExitCode   int
	Error      string
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// LogChunk is one piece of a step's output. Seq starts at 1 and has no gaps.
type LogChunk struct {
	RunID     string
	StepID    string
	Seq       int64
	Data      []byte
	CreatedAt time.Time
}

// RunUpdate changes a run. Status is required; Reason replaces the old reason when set.
// StartedAt and FinishedAt are filled by the store with the current time when the status
// moves to running or to a terminal state.
type RunUpdate struct {
	Status RunStatus
	Reason string
}

// StepUpdate changes a step. Status is required.
type StepUpdate struct {
	Status   StepStatus
	ExitCode int
	Error    string
}

// ListOptions selects a page of runs, newest first.
type ListOptions struct {
	// Limit is the page size. Zero means 50; values above 200 are capped.
	Limit int
	// Before, if set, returns only runs created before the run with this ID.
	Before string
}

func (o ListOptions) limit() int {
	switch {
	case o.Limit <= 0:
		return 50
	case o.Limit > 200:
		return 200
	}
	return o.Limit
}

// Store is the persistence boundary for the control plane. Methods are safe for concurrent use.
type Store interface {
	// CreateRun stores r as queued with one pending step per ID in stepIDs, in order. It
	// assigns the ID and creation time and returns the stored run.
	CreateRun(ctx context.Context, r Run, stepIDs []string) (Run, error)
	// GetRun returns a run and its steps in declaration order, or ErrNotFound.
	GetRun(ctx context.Context, id string) (Run, []Step, error)
	// ListRuns returns runs newest first.
	ListRuns(ctx context.Context, opts ListOptions) ([]Run, error)
	// ClaimQueuedRun atomically moves the oldest queued run to running and returns it.
	// ok is false when nothing is queued. Concurrent callers never get the same run.
	ClaimQueuedRun(ctx context.Context) (run Run, ok bool, err error)
	// UpdateRun applies a status change, or returns ErrInvalidTransition or ErrNotFound.
	UpdateRun(ctx context.Context, id string, u RunUpdate) (Run, error)
	// UpdateStep applies a status change, or returns ErrInvalidTransition or ErrNotFound.
	UpdateStep(ctx context.Context, runID, stepID string, u StepUpdate) (Step, error)
	// AppendLog adds a chunk to a step's log and returns its sequence number.
	AppendLog(ctx context.Context, runID, stepID string, data []byte) (int64, error)
	// ReadLog returns up to limit chunks with Seq greater than afterSeq, in order. A
	// non-positive limit means no limit.
	ReadLog(ctx context.Context, runID, stepID string, afterSeq int64, limit int) ([]LogChunk, error)
	// FailInterruptedRuns marks every running run failed with reason, and its unfinished
	// steps canceled. It returns how many runs it changed. Call it once at server start.
	FailInterruptedRuns(ctx context.Context, reason string) (int, error)
	Close()
}

func newID() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("store: reading random bytes: %v", err))
	}
	return hex.EncodeToString(b[:])
}

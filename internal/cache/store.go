package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/forgelinehq/forgeline/internal/executor"
)

// Store keeps one small file per successful cache key.
type Store struct {
	Dir string
}

type record struct {
	Step string    `json:"step"`
	At   time.Time `json:"at"`
}

func (s *Store) path(key string) string { return filepath.Join(s.Dir, key+".json") }

// Has reports whether a success is recorded for key.
func (s *Store) Has(key string) bool {
	_, err := os.Stat(s.path(key))
	return err == nil
}

// Put records a success for key. It writes a temporary file and renames it, so a crash never
// leaves a half-written record that looks valid.
func (s *Store) Put(key, stepID string) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(record{Step: stepID, At: time.Now().UTC()})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, "tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), s.path(key)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Executor wraps another Executor and skips steps with a recorded success for their key.
// Steps without Inputs are never cached.
type Executor struct {
	Next  executor.Executor
	Store *Store
}

// Run implements executor.Executor.
func (e Executor) Run(ctx context.Context, s executor.StepRun, logs io.Writer) (executor.Result, error) {
	// A step with secrets is never cached. The key cannot include the values (a stored hash of
	// a low-entropy secret can be guessed offline), and a key without them would serve a
	// cached success after the secret was rotated or revoked. See docs/design/005 (T3).
	if len(s.Inputs) == 0 || len(s.Secrets) > 0 {
		return e.Next.Run(ctx, s, logs)
	}
	key, err := Key(s)
	if err != nil {
		return executor.Result{}, err
	}
	if e.Store.Has(key) {
		return executor.Result{Cached: true}, nil
	}
	res, err := e.Next.Run(ctx, s, logs)
	if err == nil && res.ExitCode == 0 {
		if perr := e.Store.Put(key, s.ID); perr != nil {
			return res, fmt.Errorf("step %q: recording cache entry: %w", s.ID, perr)
		}
	}
	return res, err
}

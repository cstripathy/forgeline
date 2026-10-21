package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cstripathy/forgeline/internal/cache"
	"github.com/cstripathy/forgeline/internal/executor"
	"github.com/cstripathy/forgeline/internal/pipeline"
	"github.com/cstripathy/forgeline/internal/scheduler"
)

// runPipeline implements `forgeline run`. The workspace is the directory holding the
// pipeline file, and the cache lives in <workspace>/.forgeline/cache.
func runPipeline(args []string, stdout, stderr io.Writer, ex executor.Executor) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	parallel := fs.Int("parallel", 4, "maximum steps to run at once")
	noCache := fs.Bool("no-cache", false, "run every step, ignoring and not updating the cache")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: forgeline run [--parallel N] [--no-cache] <pipeline.yaml>")
		return 2
	}
	file := fs.Arg(0)

	p, err := pipeline.LoadFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	g, err := pipeline.Plan(p)
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	workspace, err := filepath.Abs(filepath.Dir(file))
	if err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	if !*noCache {
		ex = cache.Executor{Next: ex, Store: &cache.Store{Dir: filepath.Join(workspace, ".forgeline", "cache")}}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	logs := newPrefixLogs(stdout)
	report := scheduler.Run(ctx, g, ex, scheduler.Options{
		Parallelism: *parallel,
		Workspace:   workspace,
		Logs:        logs.writer,
	})
	logs.flush()

	fmt.Fprintf(stdout, "\npipeline %q:\n", p.Name)
	for _, s := range report.Steps {
		fmt.Fprintln(stdout, statusLine(s))
	}
	if err := report.Err(); err != nil {
		fmt.Fprintf(stderr, "forgeline: %v\n", err)
		return 1
	}
	return 0
}

func statusLine(s scheduler.StepResult) string {
	label := map[scheduler.Status]string{
		scheduler.StatusSucceeded: "ran",
		scheduler.StatusCached:    "cached",
		scheduler.StatusFailed:    "FAILED",
		scheduler.StatusSkipped:   "skipped",
		scheduler.StatusCanceled:  "canceled",
	}[s.Status]
	line := fmt.Sprintf("  %-8s %s", label, s.ID)
	switch s.Status {
	case scheduler.StatusSucceeded:
		line += fmt.Sprintf(" (%s)", s.Duration.Round(10*time.Millisecond))
	case scheduler.StatusFailed, scheduler.StatusSkipped, scheduler.StatusCanceled:
		if s.Err != nil {
			line += ": " + s.Err.Error()
		}
	}
	return line
}

// prefixLogs gives each step a writer that prefixes complete lines with "[step] " and writes
// them to one shared output under a lock, so concurrent steps do not interleave mid-line.
type prefixLogs struct {
	mu      sync.Mutex
	out     io.Writer
	writers []*prefixWriter
}

func newPrefixLogs(out io.Writer) *prefixLogs { return &prefixLogs{out: out} }

func (l *prefixLogs) writer(id string) io.Writer {
	w := &prefixWriter{parent: l, prefix: "[" + id + "] "}
	l.mu.Lock()
	l.writers = append(l.writers, w)
	l.mu.Unlock()
	return w
}

// flush writes any trailing partial lines. Call it after all steps finished.
func (l *prefixLogs) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.writers {
		if w.buf != "" {
			fmt.Fprintf(l.out, "%s%s\n", w.prefix, w.buf)
			w.buf = ""
		}
	}
}

type prefixWriter struct {
	parent *prefixLogs
	prefix string
	buf    string // guarded by parent.mu
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.parent.mu.Lock()
	defer w.parent.mu.Unlock()
	w.buf += string(p)
	for {
		i := strings.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		fmt.Fprintf(w.parent.out, "%s%s\n", w.prefix, strings.TrimSuffix(w.buf[:i], "\r"))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

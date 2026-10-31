package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/forgelinehq/forgeline/internal/pipeline"
	"github.com/forgelinehq/forgeline/internal/store"
)

// RunFailure is a plain explanation of why a run failed, worked out from what the run recorded: the
// step that failed first, its exit code and error, the end of its log, and a few well-known causes.
// It is rule-based and always available; the assistant can go further on request.
type RunFailure struct {
	Failed   bool     `json:"failed"`
	Step     string   `json:"step,omitempty"`
	Headline string   `json:"headline,omitempty"`
	ExitCode int      `json:"exit_code,omitempty"`
	Causes   []string `json:"causes"`
	Skipped  []string `json:"skipped"`
	Tail     string   `json:"tail,omitempty"`
}

const failureTailLines = 40

type failureHint struct {
	re    *regexp.Regexp
	cause string
}

var logHints = []failureHint{
	{regexp.MustCompile(`(?i)command not found|executable file not found|not found in \$PATH`), "A command is missing from the image. Use an image that has the tool, or install it in an earlier step."},
	{regexp.MustCompile(`(?i)(^|\n)\s*(--- FAIL|FAIL\b)|AssertionError|\d+ (tests? )?failed|tests? failed`), "A test failed. The lines at the end of the output say which one."},
	{regexp.MustCompile(`(?i)permission denied`), "A file or command refused access. Check who owns the file and which user the image runs as."},
	{regexp.MustCompile(`(?i)no such file or directory`), "A path does not exist. Check the working directory and that an earlier step produced the file."},
	{regexp.MustCompile(`(?i)\b401\b|\b403\b|unauthorized|authentication failed|bad credentials|access denied`), "Something rejected the credentials. Check the secret's value, and that the step lists it in permissions.secrets."},
	{regexp.MustCompile(`(?i)out of memory|\boom\b|oom-kill|cannot allocate memory`), "The step ran out of memory."},
	{regexp.MustCompile(`(?i)no space left on device`), "The disk filled up."},
}

var exitedWithCode = regexp.MustCompile(`^exited with code -?\d+$`)

var networkHint = regexp.MustCompile(`(?i)could not resolve host|temporary failure in name resolution|network is unreachable|no route to host|dial tcp|connection refused|i/o timeout|name or service not known`)

// lastRunes keeps the end of s, where the error is.
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n+1:])
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// explainFailure builds the summary for one run.
func (s *Server) explainFailure(ctx context.Context, run store.Run, steps []store.Step) RunFailure {
	out := RunFailure{Causes: []string{}, Skipped: []string{}}
	if run.Status != store.RunFailed {
		return out
	}
	out.Failed = true
	var first *store.Step
	for i := range steps {
		st := &steps[i]
		switch st.Status {
		case store.StepSkipped:
			out.Skipped = append(out.Skipped, st.StepID)
		case store.StepFailed:
			if first == nil || (st.FinishedAt != nil && (first.FinishedAt == nil || st.FinishedAt.Before(*first.FinishedAt))) {
				first = st
			}
		}
	}
	if first == nil {
		out.Headline = "The run failed before any step did"
		if run.Reason != "" {
			out.Headline += ": " + run.Reason
		}
		return out
	}
	out.Step, out.ExitCode = first.StepID, first.ExitCode
	out.Headline = fmt.Sprintf("Step %s failed", first.StepID)
	if first.ExitCode != 0 {
		out.Headline += fmt.Sprintf(" with exit code %d", first.ExitCode)
	}
	if first.Error != "" && !exitedWithCode.MatchString(first.Error) {
		out.Headline += ": " + truncate(first.Error, 200)
	}
	tail := lastLines(s.stepLogTail(ctx, run.ID, first.StepID), failureTailLines)
	out.Tail = lastRunes(tail, 4<<10)

	add := func(c string) {
		for _, have := range out.Causes {
			if have == c {
				return
			}
		}
		out.Causes = append(out.Causes, c)
	}
	switch first.ExitCode {
	case 127:
		add("Exit code 127 means the shell could not find the command. Use an image that has the tool.")
	case 126:
		add("Exit code 126 means the command was found but could not be run. Check that it is executable.")
	case 137:
		add("Exit code 137 means the step was killed, usually for using too much memory, or stopped from outside.")
	case 139:
		add("Exit code 139 means the program crashed (a segmentation fault).")
	}
	errText := strings.ToLower(first.Error)
	switch {
	case strings.Contains(errText, "timed out"):
		add("The step hit its time limit. Raise `timeout` on the step, or make it faster.")
	case strings.Contains(errText, "pull access denied"), strings.Contains(errText, "manifest unknown"), strings.Contains(errText, "no such image"), strings.Contains(errText, "unable to find image"):
		add("The image could not be pulled. Check its name, tag or digest, and that the server can reach the registry.")
	case strings.Contains(errText, "invalid secret name"), strings.Contains(errText, "preparing secrets"):
		add("A secret the step asks for is missing or can't be passed. Check Secrets and permissions.secrets.")
	}
	for _, h := range logHints {
		if h.re.MatchString(tail) {
			add(h.cause)
		}
	}
	if networkHint.MatchString(tail) {
		if stepNetworkNone(run.PipelineYAML, first.StepID) {
			add("The step could not reach the network, and it has permissions.network set to none. Set it to egress if it needs the network.")
		} else {
			add("The step could not reach something over the network. Check the address, and that the server can reach it.")
		}
	}
	if len(out.Causes) == 0 && first.ExitCode != 0 {
		add("The command ran and reported failure. Read the end of the output.")
	}
	return out
}

func stepNetworkNone(yaml, stepID string) bool {
	p, err := pipeline.Load(strings.NewReader(yaml))
	if err != nil {
		return false
	}
	for _, st := range p.Steps {
		if st.ID == stepID {
			return st.Permissions.Network == pipeline.NetworkNone
		}
	}
	return false
}

// failureCache keeps the summary of finished failed runs: their record and log no longer change, and
// working one out reads the whole log.
type failureCache struct {
	mu sync.Mutex
	m  map[string]RunFailure
}

const maxCachedFailures = 200

func (c *failureCache) get(id string) (RunFailure, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.m[id]
	return f, ok
}

func (c *failureCache) put(id string, f RunFailure) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= maxCachedFailures {
		c.m = map[string]RunFailure{}
	}
	c.m[id] = f
}

func (s *Server) handleRunFailure(w http.ResponseWriter, r *http.Request) {
	run, steps, err := s.cfg.Store.GetRun(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such run")
		return
	} else if err != nil {
		s.internalError(w, "reading the run", err)
		return
	}
	if f, ok := s.failures.get(run.ID); ok {
		writeJSON(w, http.StatusOK, f)
		return
	}
	f := s.explainFailure(r.Context(), run, steps)
	if f.Failed {
		s.failures.put(run.ID, f)
	}
	writeJSON(w, http.StatusOK, f)
}

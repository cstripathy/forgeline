package server

import (
	"strings"
	"time"

	"github.com/cstripathy/forgeline/internal/pipeline"
	"github.com/cstripathy/forgeline/internal/store"
)

// RunJSON is the API representation of a run.
type RunJSON struct {
	ID         string     `json:"id"`
	Pipeline   string     `json:"pipeline"`
	Status     string     `json:"status"`
	Reason     string     `json:"reason,omitempty"`
	Trigger    string     `json:"trigger"`
	Source     *SourceRef `json:"source,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Steps      []StepJSON `json:"steps,omitempty"`
}

// SourceRef says which repository state a run came from, when it came from one.
type SourceRef struct {
	Repo string `json:"repo,omitempty"`
	Ref  string `json:"ref,omitempty"`
	SHA  string `json:"sha,omitempty"`
}

// StepJSON is the API representation of a step.
type StepJSON struct {
	ID         string     `json:"id"`
	Needs      []string   `json:"needs,omitempty"`
	Status     string     `json:"status"`
	ExitCode   int        `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// ListJSON is a page of runs. NextBefore, when set, is the cursor for the next page.
type ListJSON struct {
	Runs       []RunJSON `json:"runs"`
	NextBefore string    `json:"next_before,omitempty"`
}

// ErrorJSON is the body of every error response.
type ErrorJSON struct {
	Error    string        `json:"error"`
	Problems []ProblemJSON `json:"problems,omitempty"`
}

// ProblemJSON locates one validation problem by field path.
type ProblemJSON struct {
	Path string `json:"path"`
	Msg  string `json:"msg"`
}

// runDetail is runJSON plus each step's dependencies, read from the stored pipeline text so
// the UI can draw the graph. If the stored text no longer parses, steps simply have no needs.
func runDetail(r store.Run, steps []store.Step) RunJSON {
	out := runJSON(r, steps)
	p, err := pipeline.Load(strings.NewReader(r.PipelineYAML))
	if err != nil {
		return out
	}
	needs := make(map[string][]string, len(p.Steps))
	for _, s := range p.Steps {
		needs[s.ID] = s.Needs
	}
	for i := range out.Steps {
		out.Steps[i].Needs = needs[out.Steps[i].ID]
	}
	return out
}

func runJSON(r store.Run, steps []store.Step) RunJSON {
	out := RunJSON{
		ID: r.ID, Pipeline: r.PipelineName, Status: string(r.Status), Reason: r.Reason,
		Trigger: string(r.Trigger), CreatedAt: r.CreatedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
	}
	if r.SourceRepo != "" || r.SourceRef != "" || r.SourceSHA != "" {
		out.Source = &SourceRef{Repo: r.SourceRepo, Ref: r.SourceRef, SHA: r.SourceSHA}
	}
	for _, s := range steps {
		out.Steps = append(out.Steps, StepJSON{ID: s.StepID, Status: string(s.Status), ExitCode: s.ExitCode,
			Error: s.Error, StartedAt: s.StartedAt, FinishedAt: s.FinishedAt})
	}
	return out
}

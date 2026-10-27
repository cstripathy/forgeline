package server

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/cstripathy/forgeline/internal/store"
)

const kindRunMeta = "runmeta"

// runMeta is what the console knows about a run beyond the run store's own record.
type runMeta struct {
	Actor      string `json:"actor"`
	PipelineID string `json:"pipeline_id,omitempty"`
	Version    int    `json:"version,omitempty"`
}

func (s *Server) recordRunMeta(ctx context.Context, runID string, m runMeta) {
	if err := s.docs.Put(context.WithoutCancel(ctx), kindRunMeta, runID, m); err != nil {
		s.log.Error("recording run metadata", "run", runID, "err", err)
	}
}

// decorate adds who started the run.
func (s *Server) decorate(ctx context.Context, r RunJSON) RunJSON {
	var m runMeta
	if err := s.docs.Get(ctx, kindRunMeta, r.ID, &m); err == nil {
		r.TriggeredBy, r.PipelineID, r.PipelineVersion = m.Actor, m.PipelineID, m.Version
	}
	return r
}

// --- overview ---

// PipelineSummary is one row of the overview: a pipeline and how it has been doing.
type PipelineSummary struct {
	Name        string `json:"name"`
	Saved       bool   `json:"saved"`
	Description string `json:"description,omitempty"`
	// Latest is the most recent run, if any.
	Latest *RunJSON `json:"latest,omitempty"`
	// Recent holds the statuses of up to the last 12 runs, oldest first, for a sparkline.
	Recent []string `json:"recent"`
	// SuccessRate is the share of finished runs among Recent that succeeded; null when none finished.
	SuccessRate *float64 `json:"success_rate"`
	// AvgSeconds is the mean duration of successful runs among Recent; null when none.
	AvgSeconds *float64 `json:"avg_seconds"`
	Runs       int      `json:"runs"`
}

// OverviewJSON is the dashboard.
type OverviewJSON struct {
	Pipelines []PipelineSummary `json:"pipelines"`
	Recent    []RunJSON         `json:"recent"`
	Totals    OverviewTotals    `json:"totals"`
}

// OverviewTotals counts runs in the window the overview covers.
type OverviewTotals struct {
	Queued    int `json:"queued"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Canceled  int `json:"canceled"`
	// Window is how many runs the totals cover (the most recent ones).
	Window int `json:"window"`
}

const overviewWindow = 200

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	runs, err := s.cfg.Store.ListRuns(r.Context(), store.ListOptions{Limit: overviewWindow})
	if err != nil {
		s.internalError(w, "listing runs", err)
		return
	}
	saved, err := s.loadPipelines(r.Context())
	if err != nil {
		s.internalError(w, "listing pipelines", err)
		return
	}
	out := OverviewJSON{Pipelines: []PipelineSummary{}, Recent: []RunJSON{}}
	out.Totals.Window = len(runs)

	byName := map[string]*PipelineSummary{}
	order := []string{}
	get := func(name string) *PipelineSummary {
		if ps, ok := byName[name]; ok {
			return ps
		}
		ps := &PipelineSummary{Name: name, Recent: []string{}}
		byName[name] = ps
		order = append(order, name)
		return ps
	}
	for _, sp := range saved {
		ps := get(sp.Name)
		ps.Saved, ps.Description = true, sp.Description
	}

	// runs are newest first; walk oldest first so Recent reads left to right in time.
	for i := len(runs) - 1; i >= 0; i-- {
		run := runs[i]
		ps := get(run.PipelineName)
		ps.Runs++
		ps.Recent = append(ps.Recent, string(run.Status))
		if len(ps.Recent) > 12 {
			ps.Recent = ps.Recent[1:]
		}
		rj := s.decorate(r.Context(), runJSON(run, nil))
		ps.Latest = &rj
	}
	// Rates over the last 12 runs only, so they track recent health rather than all time.
	last12 := map[string][]store.Run{}
	for _, run := range runs { // newest first
		if len(last12[run.PipelineName]) < 12 {
			last12[run.PipelineName] = append(last12[run.PipelineName], run)
		}
	}
	for name, rs := range last12 {
		var finished, ok, durN int
		var dur time.Duration
		for _, run := range rs {
			switch run.Status {
			case store.RunSucceeded:
				finished++
				ok++
				if run.StartedAt != nil && run.FinishedAt != nil {
					dur += run.FinishedAt.Sub(*run.StartedAt)
					durN++
				}
			case store.RunFailed:
				finished++
			}
		}
		ps := byName[name]
		if finished > 0 {
			v := float64(ok) / float64(finished)
			ps.SuccessRate = &v
		}
		if durN > 0 {
			v := dur.Seconds() / float64(durN)
			ps.AvgSeconds = &v
		}
	}
	for _, run := range runs {
		switch run.Status {
		case store.RunQueued:
			out.Totals.Queued++
		case store.RunRunning:
			out.Totals.Running++
		case store.RunSucceeded:
			out.Totals.Succeeded++
		case store.RunFailed:
			out.Totals.Failed++
		case store.RunCanceled:
			out.Totals.Canceled++
		}
	}
	for i, run := range runs {
		if i == 15 {
			break
		}
		out.Recent = append(out.Recent, s.decorate(r.Context(), runJSON(run, nil)))
	}
	// Pipelines that are failing or running first, then by name, so attention goes where it is needed.
	rank := func(ps *PipelineSummary) int {
		if ps.Latest == nil {
			return 4
		}
		switch ps.Latest.Status {
		case "running":
			return 0
		case "failed":
			return 1
		case "queued":
			return 2
		}
		return 3
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := byName[order[i]], byName[order[j]]
		if rank(a) != rank(b) {
			return rank(a) < rank(b)
		}
		return a.Name < b.Name
	})
	for _, name := range order {
		out.Pipelines = append(out.Pipelines, *byName[name])
	}
	writeJSON(w, http.StatusOK, out)
}

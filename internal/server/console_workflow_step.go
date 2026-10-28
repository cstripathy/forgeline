package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/forgelinehq/forgeline/internal/authn"
	"github.com/forgelinehq/forgeline/internal/pipeline"
	"github.com/forgelinehq/forgeline/internal/store"
)

// A pipeline step that starts a GitHub Actions workflow and waits for it (design note 011,
// slice 2). It runs here, on the server, because it needs the GitHub credentials, which never
// enter a container. The same limits as the Actions page apply, checked again when the step runs:
// the repository must be on the allow list, the branch must be one its rule allows, inputs are
// bounded, and starts share the per-minute limit. Runs started by GitHub cannot use it.

var (
	// workflowPoll is how often a waiting step asks GitHub for the run's state. Tests shorten it.
	workflowPoll = 10 * time.Second
	// workflowFindWait is how long a step looks for the run it just started, when GitHub's answer
	// to the dispatch did not name it.
	workflowFindWait = 90 * time.Second
	workflowMaxErrs  = 4
)

// workflowDeniedFor says why a run may not use workflow steps, or "".
func workflowDeniedFor(run store.Run) string {
	if run.Trigger == store.TriggerGitHub {
		return "runs started by GitHub may not start GitHub workflows (the pipeline came from a push or pull request)"
	}
	return ""
}

// workflowStep returns the function the scheduler calls for a workflow step of run.
func (s *Server) workflowStep(run store.Run) func(context.Context, *pipeline.Step, io.Writer) error {
	return func(ctx context.Context, st *pipeline.Step, logs io.Writer) error {
		who := Principal{Name: "run " + run.ID, Role: authn.Developer}
		audit := func(allowed bool, detail string) {
			s.audit(ctx, who, "", "actions.step", "github/"+st.Workflow.Repo, detail, allowed)
		}
		err := s.runWorkflowStep(ctx, st.Workflow, logs, audit)
		return err
	}
}

func (s *Server) runWorkflowStep(ctx context.Context, w *pipeline.Workflow, logs io.Writer, audit func(bool, string)) error {
	say := func(format string, a ...any) { fmt.Fprintf(logs, format+"\n", a...) }
	gh := s.gh()
	if gh == nil {
		return errors.New("GitHub is not connected on this server")
	}
	// The allow list and the branch rule are read now, not when the pipeline was saved.
	var rule RepoRule
	found := false
	for _, r := range gh.Allow {
		if strings.EqualFold(r.Repo, w.Repo) {
			rule, found = r, true
		}
	}
	if !found {
		audit(false, "refused: "+w.Repo+" is not on the GitHub allow list")
		return fmt.Errorf("%s is not on the GitHub allow list", w.Repo)
	}
	if strings.HasPrefix(w.Ref, "refs/") || !gh.allowed(rule.Repo, w.Ref) {
		audit(false, "refused: branch "+truncate(w.Ref, 80)+" is not allowed")
		return fmt.Errorf("branch %q is not allowed for %s (see the GitHub rules under Integrations)", w.Ref, rule.Repo)
	}
	if !workflowIDRE.MatchString(w.Workflow) && !workflowFileRE.MatchString(w.Workflow) {
		return fmt.Errorf("bad workflow %q", w.Workflow)
	}
	if len(w.Inputs) > maxDispatchInputs {
		return fmt.Errorf("at most %d inputs", maxDispatchInputs)
	}
	for k, v := range w.Inputs {
		if !inputNameRE.MatchString(k) || len(v) > maxInputValue {
			return fmt.Errorf("input %q has a bad name or a value over %d bytes", k, maxInputValue)
		}
	}
	if !s.actionsLimiter.allow("workflow steps") {
		return errors.New("too many workflow starts in the last minute; the step was not started")
	}

	wf := url.PathEscape(w.Workflow)
	base := "/repos/" + rule.Repo + "/actions/workflows/" + wf
	// Remember the runs that exist, so the one this step starts can be told apart from them.
	before := map[int64]bool{}
	if !w.Detach {
		var existing struct {
			Runs []ghRun `json:"workflow_runs"`
		}
		q := url.Values{"event": {"workflow_dispatch"}, "branch": {w.Ref}, "per_page": {"10"}}
		if _, err := s.ghActionsCall(ctx, gh, http.MethodGet, base+"/runs?"+q.Encode(), nil, &existing); err != nil {
			return err
		}
		for _, r := range existing.Runs {
			before[r.ID] = true
		}
	}
	body := map[string]any{"ref": w.Ref}
	if len(w.Inputs) > 0 {
		body["inputs"] = w.Inputs
	}
	var res struct {
		RunID   int64  `json:"workflow_run_id"`
		HTMLURL string `json:"html_url"`
	}
	if _, err := s.ghActionsCall(ctx, gh, http.MethodPost, base+"/dispatches", body, &res); err != nil {
		audit(false, "failed: "+w.Workflow+" on "+w.Ref+": "+err.Error())
		return err
	}
	audit(true, fmt.Sprintf("started %s on %s with %d inputs", w.Workflow, w.Ref, len(w.Inputs)))
	say("Started %s on %s of %s.", w.Workflow, w.Ref, rule.Repo)
	if w.Detach {
		say("Not waiting (detach is set).")
		return nil
	}

	runID, link := res.RunID, safeLink(res.HTMLURL)
	if runID == 0 {
		var err error
		if runID, link, err = s.findStartedRun(ctx, gh, rule.Repo, base, w.Ref, before); err != nil {
			return err
		}
	}
	if link != "" {
		say("Run %d: %s", runID, link)
	} else {
		say("Run %d", runID)
	}
	return s.waitForWorkflowRun(ctx, gh, rule.Repo, runID, say)
}

// findStartedRun looks for the run that appeared after the dispatch.
func (s *Server) findStartedRun(ctx context.Context, gh *GitHubConfig, repo, base, ref string, before map[int64]bool) (int64, string, error) {
	deadline := time.Now().Add(workflowFindWait)
	for {
		var list struct {
			Runs []ghRun `json:"workflow_runs"`
		}
		q := url.Values{"event": {"workflow_dispatch"}, "branch": {ref}, "per_page": {"10"}}
		if _, err := s.ghActionsCall(ctx, gh, http.MethodGet, base+"/runs?"+q.Encode(), nil, &list); err != nil {
			return 0, "", err
		}
		for _, r := range list.Runs {
			if !before[r.ID] {
				return r.ID, safeLink(r.HTMLURL), nil
			}
		}
		if time.Now().After(deadline) {
			return 0, "", errors.New("GitHub accepted the request, but no new run appeared (does the workflow have on: workflow_dispatch, and is the file on that branch?)")
		}
		select {
		case <-time.After(workflowPoll):
		case <-ctx.Done():
			return 0, "", ctx.Err()
		}
	}
}

// waitForWorkflowRun polls until the run completes. If the step is canceled or times out, the
// GitHub run is canceled too, so work this server started does not keep running unseen.
func (s *Server) waitForWorkflowRun(ctx context.Context, gh *GitHubConfig, repo string, id int64, say func(string, ...any)) error {
	path := fmt.Sprintf("/repos/%s/actions/runs/%d", repo, id)
	last, errs := "", 0
	for {
		var r ghRun
		if _, err := s.ghActionsCall(ctx, gh, http.MethodGet, path, nil, &r); err != nil {
			if ctx.Err() != nil {
				break
			}
			if errs++; errs >= workflowMaxErrs {
				return fmt.Errorf("lost contact with GitHub while waiting: %w", err)
			}
		} else {
			errs = 0
			if r.Status != last {
				say("GitHub says: %s", r.Status)
				last = r.Status
			}
			if r.Status == "completed" {
				if r.Conclusion == "success" {
					say("Workflow succeeded.")
					return nil
				}
				return fmt.Errorf("workflow finished with conclusion %q%s", r.Conclusion, linkSuffix(r.HTMLURL))
			}
		}
		select {
		case <-time.After(workflowPoll):
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	// Stopped from outside: stop the run on GitHub as well, best effort.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if _, err := s.ghActionsCall(cctx, gh, http.MethodPost, path+"/cancel", nil, nil); err != nil {
		say("Could not cancel the GitHub run: %v", err)
	} else {
		say("Canceled the GitHub run.")
	}
	return ctx.Err()
}

func linkSuffix(raw string) string {
	if l := safeLink(raw); l != "" {
		return ": " + l
	}
	return ""
}

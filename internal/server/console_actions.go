package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// GitHub Actions through the API (design note 011). The console can list workflows and runs of
// the repositories on the GitHub allow list, start a workflow, cancel or re-run a run, and read
// a job's log. It never runs anything itself and holds no runner credentials.
//
// Every call is limited to repositories on the allow list. A workflow can only be started on a
// branch the rule allows. The GitHub token goes only to the GitHub API, never to the address a
// log is downloaded from. Log text is untrusted and is returned as plain text.

const (
	actionsTimeout    = 30 * time.Second
	maxActionsJSON    = 4 << 20
	maxLogDownload    = 8 << 20
	logTailBytes      = 256 << 10
	maxDispatchInputs = 25 // GitHub's own limit
	maxInputValue     = 1000
	actionsPerPage    = 30
)

var (
	workflowFileRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}\.ya?ml$`)
	workflowIDRE   = regexp.MustCompile(`^[0-9]{1,18}$`)
	numericIDRE    = regexp.MustCompile(`^[0-9]{1,18}$`)
	inputNameRE    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	runStatusRE    = regexp.MustCompile(`^(queued|in_progress|completed|success|failure|cancelled|waiting|requested|pending)$`)
)

// ActionsRepoJSON is a repository the console may use, with the branches a workflow may be started on.
type ActionsRepoJSON struct {
	Repo     string   `json:"repo"`
	Branches []string `json:"branches"`
}

// WorkflowJSON is a workflow file in a repository.
type WorkflowJSON struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url,omitempty"`
}

// WorkflowRunJSON is one run of a workflow.
type WorkflowRunJSON struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	WorkflowID int64  `json:"workflow_id"`
	Number     int    `json:"run_number"`
	Attempt    int    `json:"run_attempt"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	Event      string `json:"event"`
	Branch     string `json:"branch"`
	SHA        string `json:"sha"`
	Actor      string `json:"actor,omitempty"`
	// Fork is true when the run's code comes from another repository (a pull request from a fork).
	Fork      bool   `json:"fork"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	HTMLURL   string `json:"html_url,omitempty"`
}

// WorkflowStepJSON is one step of a job.
type WorkflowStepJSON struct {
	Number     int    `json:"number"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
}

// WorkflowJobJSON is one job of a run.
type WorkflowJobJSON struct {
	ID          int64              `json:"id"`
	Name        string             `json:"name"`
	Status      string             `json:"status"`
	Conclusion  string             `json:"conclusion,omitempty"`
	StartedAt   string             `json:"started_at,omitempty"`
	CompletedAt string             `json:"completed_at,omitempty"`
	RunnerName  string             `json:"runner_name,omitempty"`
	Labels      []string           `json:"labels,omitempty"`
	Steps       []WorkflowStepJSON `json:"steps"`
}

// actionsRepo checks that repo may be used and returns the GitHub settings and the rule's spelling
// of the name. It writes the error response itself.
func (s *Server) actionsRepo(w http.ResponseWriter, repo string) (*GitHubConfig, RepoRule, bool) {
	gh := s.gh()
	if gh == nil {
		writeError(w, http.StatusConflict, "connect GitHub under Integrations first")
		return nil, RepoRule{}, false
	}
	if !repoNameRE.MatchString(repo) {
		writeError(w, http.StatusBadRequest, "the repository must look like owner/name")
		return nil, RepoRule{}, false
	}
	for _, rule := range gh.Allow {
		if strings.EqualFold(rule.Repo, repo) {
			return gh, rule, true
		}
	}
	writeError(w, http.StatusForbidden, "that repository is not on the GitHub allow list")
	return nil, RepoRule{}, false
}

// ghActionsCall calls the GitHub API with the integration's token and decodes a JSON answer into out.
// It returns GitHub's status code. A non-2xx answer becomes an error a person can act on.
func (s *Server) ghActionsCall(ctx context.Context, gh *GitHubConfig, method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, actionsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, gh.APIBase+path, rd)
	if err != nil {
		return 0, err
	}
	if err := s.githubHeaders(req); err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := gh.HTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxActionsJSON))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, githubError(resp.StatusCode, raw)
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, errors.New("GitHub sent an answer that could not be read")
		}
	}
	return resp.StatusCode, nil
}

// githubError turns a failed GitHub answer into a short message.
func githubError(status int, raw []byte) error {
	var e struct {
		Message string `json:"message"`
	}
	json.Unmarshal(raw, &e)
	msg := truncate(firstLine(e.Message), 200)
	switch {
	case status == http.StatusForbidden && strings.Contains(strings.ToLower(msg), "not accessible by integration"):
		return errors.New("GitHub refused: the GitHub App does not have the Actions permission yet. Approve it on the App's page, then try again")
	case status == http.StatusUnauthorized:
		return errors.New("GitHub rejected the stored credentials; reconnect GitHub under Integrations")
	case status == http.StatusNotFound:
		return errors.New("GitHub answered not found (the repository, workflow or run does not exist, or the token cannot see it)")
	case status == http.StatusUnprocessableEntity && msg != "":
		return fmt.Errorf("GitHub refused: %s", msg)
	case msg != "":
		return fmt.Errorf("GitHub answered %d: %s", status, msg)
	}
	return fmt.Errorf("GitHub answered %d", status)
}

func safeLink(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return raw
}

func (s *Server) handleActionsRepos(w http.ResponseWriter, r *http.Request) {
	gh := s.gh()
	out := []ActionsRepoJSON{}
	if gh != nil {
		for _, rule := range gh.Allow {
			out = append(out, ActionsRepoJSON{Repo: rule.Repo, Branches: rule.Branches})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"connected": gh != nil, "repos": out})
}

func (s *Server) handleActionsWorkflows(w http.ResponseWriter, r *http.Request) {
	gh, rule, ok := s.actionsRepo(w, r.URL.Query().Get("repo"))
	if !ok {
		return
	}
	var resp struct {
		Workflows []struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Path    string `json:"path"`
			State   string `json:"state"`
			HTMLURL string `json:"html_url"`
		} `json:"workflows"`
	}
	if _, err := s.ghActionsCall(r.Context(), gh, http.MethodGet, "/repos/"+rule.Repo+"/actions/workflows?per_page=100", nil, &resp); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := make([]WorkflowJSON, 0, len(resp.Workflows))
	for _, x := range resp.Workflows {
		out = append(out, WorkflowJSON{ID: x.ID, Name: truncate(x.Name, 200), Path: truncate(x.Path, 300), State: x.State, HTMLURL: safeLink(x.HTMLURL)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": rule.Repo, "workflows": out})
}

type ghRun struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	DisplayTitle string `json:"display_title"`
	WorkflowID   int64  `json:"workflow_id"`
	RunNumber    int    `json:"run_number"`
	RunAttempt   int    `json:"run_attempt"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	Event        string `json:"event"`
	HeadBranch   string `json:"head_branch"`
	HeadSHA      string `json:"head_sha"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	HTMLURL      string `json:"html_url"`
	Actor        struct {
		Login string `json:"login"`
	} `json:"actor"`
	HeadRepository struct {
		FullName string `json:"full_name"`
		Fork     bool   `json:"fork"`
	} `json:"head_repository"`
}

func actionsRunView(repo string, x ghRun) WorkflowRunJSON {
	name := x.DisplayTitle
	if name == "" {
		name = x.Name
	}
	fork := x.HeadRepository.Fork || (x.HeadRepository.FullName != "" && !strings.EqualFold(x.HeadRepository.FullName, repo))
	return WorkflowRunJSON{ID: x.ID, Name: truncate(name, 200), WorkflowID: x.WorkflowID, Number: x.RunNumber, Attempt: x.RunAttempt,
		Status: x.Status, Conclusion: x.Conclusion, Event: x.Event, Branch: truncate(x.HeadBranch, 200), SHA: x.HeadSHA,
		Actor: truncate(x.Actor.Login, 100), Fork: fork, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt, HTMLURL: safeLink(x.HTMLURL)}
}

func (s *Server) handleActionsRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	gh, rule, ok := s.actionsRepo(w, q.Get("repo"))
	if !ok {
		return
	}
	path := "/repos/" + rule.Repo + "/actions/runs"
	if wf := q.Get("workflow"); wf != "" {
		if !workflowIDRE.MatchString(wf) && !workflowFileRE.MatchString(wf) {
			writeError(w, http.StatusBadRequest, "the workflow must be its number or its file name such as ci.yml")
			return
		}
		path = "/repos/" + rule.Repo + "/actions/workflows/" + url.PathEscape(wf) + "/runs"
	}
	params := url.Values{"per_page": {strconv.Itoa(actionsPerPage)}}
	if st := q.Get("status"); st != "" {
		if !runStatusRE.MatchString(st) {
			writeError(w, http.StatusBadRequest, "unknown status filter")
			return
		}
		params.Set("status", st)
	}
	var resp struct {
		Runs []ghRun `json:"workflow_runs"`
	}
	if _, err := s.ghActionsCall(r.Context(), gh, http.MethodGet, path+"?"+params.Encode(), nil, &resp); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := make([]WorkflowRunJSON, 0, len(resp.Runs))
	for _, x := range resp.Runs {
		out = append(out, actionsRunView(rule.Repo, x))
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": rule.Repo, "runs": out})
}

func (s *Server) handleActionsRun(w http.ResponseWriter, r *http.Request) {
	gh, rule, ok := s.actionsRepo(w, r.URL.Query().Get("repo"))
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !numericIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "bad run id")
		return
	}
	var run ghRun
	if _, err := s.ghActionsCall(r.Context(), gh, http.MethodGet, "/repos/"+rule.Repo+"/actions/runs/"+id, nil, &run); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	var jobs struct {
		Jobs []struct {
			ID          int64    `json:"id"`
			Name        string   `json:"name"`
			Status      string   `json:"status"`
			Conclusion  string   `json:"conclusion"`
			StartedAt   string   `json:"started_at"`
			CompletedAt string   `json:"completed_at"`
			RunnerName  string   `json:"runner_name"`
			Labels      []string `json:"labels"`
			Steps       []struct {
				Number     int    `json:"number"`
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if _, err := s.ghActionsCall(r.Context(), gh, http.MethodGet, "/repos/"+rule.Repo+"/actions/runs/"+id+"/jobs?per_page=100", nil, &jobs); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := make([]WorkflowJobJSON, 0, len(jobs.Jobs))
	for _, j := range jobs.Jobs {
		job := WorkflowJobJSON{ID: j.ID, Name: truncate(j.Name, 200), Status: j.Status, Conclusion: j.Conclusion, StartedAt: j.StartedAt,
			CompletedAt: j.CompletedAt, RunnerName: truncate(j.RunnerName, 100), Steps: []WorkflowStepJSON{}}
		for _, l := range j.Labels {
			job.Labels = append(job.Labels, truncate(l, 60))
		}
		for _, st := range j.Steps {
			job.Steps = append(job.Steps, WorkflowStepJSON{Number: st.Number, Name: truncate(st.Name, 200), Status: st.Status, Conclusion: st.Conclusion})
		}
		out = append(out, job)
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": rule.Repo, "run": actionsRunView(rule.Repo, run), "jobs": out})
}

type dispatchRequest struct {
	Repo     string            `json:"repo"`
	Workflow string            `json:"workflow"`
	Ref      string            `json:"ref"`
	Inputs   map[string]string `json:"inputs"`
}

func (s *Server) handleActionsDispatch(w http.ResponseWriter, r *http.Request) {
	var req dispatchRequest
	if !readJSON(w, r, &req) {
		return
	}
	gh, rule, ok := s.actionsRepo(w, req.Repo)
	if !ok {
		return
	}
	p := principalFrom(r.Context())
	target := "github/" + rule.Repo
	if !workflowIDRE.MatchString(req.Workflow) && !workflowFileRE.MatchString(req.Workflow) {
		writeError(w, http.StatusBadRequest, "the workflow must be its number or its file name such as ci.yml")
		return
	}
	// Only a branch the rule allows: starting a workflow runs the workflow file of that ref, so
	// the ref is as trusted as a push to it would be.
	if strings.HasPrefix(req.Ref, "refs/") || !gh.allowed(rule.Repo, req.Ref) {
		s.auditReq(r, "actions.dispatch", target, "refused: branch "+truncate(req.Ref, 80)+" is not allowed", false)
		writeError(w, http.StatusForbidden, "that branch is not allowed for this repository (see the GitHub rules under Integrations)")
		return
	}
	if len(req.Inputs) > maxDispatchInputs {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d inputs", maxDispatchInputs))
		return
	}
	for k, v := range req.Inputs {
		if !inputNameRE.MatchString(k) || len(v) > maxInputValue || !utf8.ValidString(v) {
			writeError(w, http.StatusBadRequest, "an input has a bad name or a value over 1000 bytes")
			return
		}
	}
	if !s.actionsLimiter.allow(p.Name) {
		writeError(w, http.StatusTooManyRequests, "too many actions; wait a minute")
		return
	}
	body := map[string]any{"ref": req.Ref}
	if len(req.Inputs) > 0 {
		body["inputs"] = req.Inputs
	}
	var res struct {
		RunID   int64  `json:"workflow_run_id"`
		HTMLURL string `json:"html_url"`
	}
	if _, err := s.ghActionsCall(r.Context(), gh, http.MethodPost, "/repos/"+rule.Repo+"/actions/workflows/"+url.PathEscape(req.Workflow)+"/dispatches", body, &res); err != nil {
		s.auditReq(r, "actions.dispatch", target, "failed: "+req.Workflow+" on "+req.Ref+": "+err.Error(), false)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.auditReq(r, "actions.dispatch", target, fmt.Sprintf("started %s on %s with %d inputs", req.Workflow, req.Ref, len(req.Inputs)), true)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "run_id": res.RunID, "html_url": safeLink(res.HTMLURL)})
}

type runActionRequest struct {
	Repo       string `json:"repo"`
	FailedOnly bool   `json:"failed_only"`
}

// handleActionsRunAction cancels or re-runs a run.
func (s *Server) handleActionsRunAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req runActionRequest
		if !readJSON(w, r, &req) {
			return
		}
		gh, rule, ok := s.actionsRepo(w, req.Repo)
		if !ok {
			return
		}
		id := r.PathValue("id")
		if !numericIDRE.MatchString(id) {
			writeError(w, http.StatusBadRequest, "bad run id")
			return
		}
		p := principalFrom(r.Context())
		if !s.actionsLimiter.allow(p.Name) {
			writeError(w, http.StatusTooManyRequests, "too many actions; wait a minute")
			return
		}
		path := "/repos/" + rule.Repo + "/actions/runs/" + id + "/" + action
		if action == "rerun" && req.FailedOnly {
			path = "/repos/" + rule.Repo + "/actions/runs/" + id + "/rerun-failed-jobs"
		}
		target := "github/" + rule.Repo
		if _, err := s.ghActionsCall(r.Context(), gh, http.MethodPost, path, nil, nil); err != nil {
			s.auditReq(r, "actions."+action, target, "failed: run "+id+": "+err.Error(), false)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		s.auditReq(r, "actions."+action, target, "run "+id, true)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// handleActionsLogs returns the end of a job's log as plain text. GitHub answers with a
// redirect to a short-lived download address. The token is sent only to GitHub's API: the
// redirect is followed by hand, without credentials, and only to a web address.
func (s *Server) handleActionsLogs(w http.ResponseWriter, r *http.Request) {
	gh, rule, ok := s.actionsRepo(w, r.URL.Query().Get("repo"))
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !numericIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "bad job id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gh.APIBase+"/repos/"+rule.Repo+"/actions/jobs/"+id+"/logs", nil)
	if err != nil {
		s.internalError(w, "building the request", err)
		return
	}
	if err := s.githubHeaders(req); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	noFollow := *gh.HTTPClient
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noFollow.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not reach GitHub")
		return
	}
	defer resp.Body.Close()
	var body io.Reader = resp.Body
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusTemporaryRedirect || resp.StatusCode == http.StatusSeeOther {
		loc, err := checkLogURL(gh.APIBase, resp.Header.Get("Location"))
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		dl, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil) // no Authorization header
		if err != nil {
			writeError(w, http.StatusBadGateway, "bad log address")
			return
		}
		dl.Header.Set("User-Agent", "forgeline")
		r2, err := noFollow.Do(dl)
		if err != nil {
			writeError(w, http.StatusBadGateway, "could not download the log")
			return
		}
		defer r2.Body.Close()
		if r2.StatusCode != http.StatusOK {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("the log download answered %d (logs expire after a while)", r2.StatusCode))
			return
		}
		body = r2.Body
	} else if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		writeError(w, http.StatusBadGateway, githubError(resp.StatusCode, raw).Error())
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(body, maxLogDownload))
	truncated := len(raw) >= maxLogDownload
	if len(raw) > logTailBytes {
		raw, truncated = raw[len(raw)-logTailBytes:], true
	}
	text := strings.ToValidUTF8(string(raw), "�")
	s.auditReq(r, "actions.logs", "github/"+rule.Repo, "job "+id, true)
	writeJSON(w, http.StatusOK, map[string]any{"job_id": id, "truncated": truncated, "text": text})
}

// checkLogURL accepts the address GitHub redirects a log download to: a web address with no
// credentials in it. A bare IP address is refused unless the API itself is on loopback (tests).
func checkLogURL(apiBase, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New("GitHub sent a log address that is not a web link")
	}
	apiLoop := false
	if a, err := url.Parse(apiBase); err == nil {
		apiLoop = isLoopbackHost(a.Hostname())
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil && !apiLoop {
		return "", errors.New("GitHub sent a log address that is a bare IP number")
	}
	if u.Scheme == "http" && !(apiLoop && isLoopbackHost(host)) {
		return "", errors.New("GitHub sent a log address that is not https")
	}
	return u.String(), nil
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

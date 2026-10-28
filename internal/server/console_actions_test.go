package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/forgelinehq/forgeline/internal/executor"
)

// actionsGH stands in for the parts of GitHub's API the Actions panel calls, and records what it
// was asked, with which credentials.
type actionsGH struct {
	srv  *httptest.Server
	dl   *httptest.Server // where log downloads are redirected to
	mu   sync.Mutex
	hits []string // "METHOD path"
	auth map[string]string
	body map[string]string // last body per "METHOD path"
	// answers to override: path -> status
	status map[string]int
	log    string
	redir  string // Location to send for the log request; empty means the download server
	dlAuth []string
}

func newActionsGH(t *testing.T) *actionsGH {
	g := &actionsGH{auth: map[string]string{}, body: map[string]string{}, status: map[string]int{}, log: "line one\nline two\n"}
	g.dl = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.dlAuth = append(g.dlAuth, r.Header.Get("Authorization"))
		g.mu.Unlock()
		io.WriteString(w, g.log)
	}))
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		g.mu.Lock()
		g.hits = append(g.hits, key)
		g.auth[key] = r.Header.Get("Authorization")
		g.body[key] = string(b)
		st := g.status[r.URL.Path]
		redir := g.redir
		g.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/workflows") && r.Method == "GET":
			io.WriteString(w, `{"workflows":[{"id":11,"name":"CI","path":".github/workflows/ci.yml","state":"active","html_url":"https://github.com/acme/widgets/actions/workflows/ci.yml"}]}`)
		case strings.HasSuffix(r.URL.Path, "/dispatches"):
			w.WriteHeader(204)
		case r.URL.Path == "/repos/acme/widgets/actions/runs" || strings.HasSuffix(r.URL.Path, "/workflows/ci.yml/runs"):
			io.WriteString(w, `{"workflow_runs":[
			 {"id":901,"name":"CI","display_title":"Fix <b>bug</b>","workflow_id":11,"run_number":7,"run_attempt":1,"status":"completed","conclusion":"failure","event":"push","head_branch":"main","head_sha":"`+sha1st+`","actor":{"login":"ada"},"head_repository":{"full_name":"acme/widgets","fork":false},"html_url":"javascript://github.com/%0Aalert(1)"},
			 {"id":902,"name":"CI","workflow_id":11,"run_number":8,"status":"queued","event":"pull_request","head_branch":"patch-1","head_sha":"`+sha1st+`","actor":{"login":"mallory"},"head_repository":{"full_name":"mallory/widgets","fork":true},"html_url":"https://github.com/acme/widgets/actions/runs/902"}]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs/901/jobs"):
			io.WriteString(w, `{"jobs":[{"id":5001,"name":"test","status":"completed","conclusion":"failure","runner_name":"GitHub Actions 1","labels":["ubuntu-latest"],"steps":[{"number":1,"name":"Set up job","status":"completed","conclusion":"success"},{"number":2,"name":"go test","status":"completed","conclusion":"failure"}]}]}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runs/901"):
			io.WriteString(w, `{"id":901,"name":"CI","workflow_id":11,"run_number":7,"status":"completed","conclusion":"failure","event":"push","head_branch":"main","head_sha":"`+sha1st+`","head_repository":{"full_name":"acme/widgets"}}`)
		case strings.HasSuffix(r.URL.Path, "/logs"):
			loc := redir
			if loc == "" {
				loc = g.dl.URL + "/blob?sig=abc"
			}
			w.Header().Set("Location", loc)
			w.WriteHeader(302)
		case strings.HasSuffix(r.URL.Path, "/cancel"), strings.HasSuffix(r.URL.Path, "/rerun"), strings.HasSuffix(r.URL.Path, "/rerun-failed-jobs"):
			w.WriteHeader(202)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)
	t.Cleanup(g.dl.Close)
	return g
}

func (g *actionsGH) calls(substr string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, h := range g.hits {
		if strings.Contains(h, substr) {
			n++
		}
	}
	return n
}

func connectActions(t *testing.T, h *harness, g *actionsGH) *browser {
	t.Helper()
	admin := h.adminBrowser("ada")
	admin.expect(200, "PUT", "/v1/integrations/github", map[string]any{
		"enabled": true, "api_base": g.srv.URL, "public_url": "https://ci.example.com",
		"rules": []map[string]any{{"repo": ghRepo, "branches": []string{"main", "release/*"}}},
		"token": ghToken, "generate_webhook_secret": true,
	})
	return admin
}

func TestActionsListsWorkflowsAndRunsAndNeverTrustsGitHubText(t *testing.T) {
	g := newActionsGH(t)
	h := newHarness(t, &executor.Fake{})
	admin := connectActions(t, h, g)

	var wf struct{ Workflows []WorkflowJSON }
	json.Unmarshal(admin.expect(200, "GET", "/v1/actions/workflows?repo=ACME/Widgets", nil), &wf)
	if len(wf.Workflows) != 1 || wf.Workflows[0].Name != "CI" {
		t.Fatalf("workflows = %+v", wf)
	}
	if got := g.auth["GET /repos/acme/widgets/actions/workflows"]; got != "Bearer "+ghToken {
		t.Errorf("GitHub was called with %q", got)
	}
	var runs struct{ Runs []WorkflowRunJSON }
	json.Unmarshal(admin.expect(200, "GET", "/v1/actions/runs?repo="+ghRepo+"&workflow=ci.yml&status=completed", nil), &runs)
	if len(runs.Runs) != 2 {
		t.Fatalf("runs = %+v", runs)
	}
	if runs.Runs[0].Fork || !runs.Runs[1].Fork {
		t.Errorf("fork flags wrong: %+v", runs.Runs)
	}
	if runs.Runs[0].HTMLURL != "" {
		t.Errorf("a javascript: link was passed through: %q", runs.Runs[0].HTMLURL)
	}
	if runs.Runs[0].Name != "Fix <b>bug</b>" { // text stays text; the page inserts it as text
		t.Errorf("name = %q", runs.Runs[0].Name)
	}

	var run struct {
		Run  WorkflowRunJSON
		Jobs []WorkflowJobJSON
	}
	json.Unmarshal(admin.expect(200, "GET", "/v1/actions/runs/901?repo="+ghRepo, nil), &run)
	if len(run.Jobs) != 1 || len(run.Jobs[0].Steps) != 2 || run.Jobs[0].Steps[1].Conclusion != "failure" {
		t.Fatalf("jobs = %+v", run.Jobs)
	}
}

func TestActionsDispatchOnlyOnAllowedRepoAndBranchWithCheckedInputs(t *testing.T) {
	g := newActionsGH(t)
	h := newHarness(t, &executor.Fake{})
	admin := connectActions(t, h, g)
	dispatch := func(m map[string]any) (*http.Response, []byte) { return admin.do("POST", "/v1/actions/dispatch", m) }

	admin.expect(200, "POST", "/v1/actions/dispatch", map[string]any{"repo": ghRepo, "workflow": "ci.yml", "ref": "main", "inputs": map[string]string{"env": "staging"}})
	if body := g.body["POST /repos/acme/widgets/actions/workflows/ci.yml/dispatches"]; !strings.Contains(body, `"ref":"main"`) || !strings.Contains(body, `"env":"staging"`) {
		t.Errorf("GitHub got %s", body)
	}
	admin.expect(200, "POST", "/v1/actions/dispatch", map[string]any{"repo": ghRepo, "workflow": "11", "ref": "release/1.2"})
	before := g.calls("dispatches")

	refused := map[string]map[string]any{
		"another repository":     {"repo": "evil/repo", "workflow": "ci.yml", "ref": "main"},
		"a branch not allowed":   {"repo": ghRepo, "workflow": "ci.yml", "ref": "dev"},
		"a full ref":             {"repo": ghRepo, "workflow": "ci.yml", "ref": "refs/heads/main"},
		"a tag-like ref":         {"repo": ghRepo, "workflow": "ci.yml", "ref": "refs/tags/v1"},
		"a path in the workflow": {"repo": ghRepo, "workflow": "../../x.yml", "ref": "main"},
		"a slash in workflow":    {"repo": ghRepo, "workflow": "a/b.yml", "ref": "main"},
		"a long input":           {"repo": ghRepo, "workflow": "ci.yml", "ref": "main", "inputs": map[string]string{"x": strings.Repeat("a", 1001)}},
		"a bad input name":       {"repo": ghRepo, "workflow": "ci.yml", "ref": "main", "inputs": map[string]string{"a b": "1"}},
	}
	for name, req := range refused {
		resp, body := dispatch(req)
		if resp.StatusCode != 400 && resp.StatusCode != 403 {
			t.Errorf("%s: status %d, want 400 or 403: %s", name, resp.StatusCode, body)
		}
	}
	many := map[string]string{}
	for i := 0; i < 26; i++ {
		many[fmt.Sprintf("k%d", i)] = "v"
	}
	if resp, _ := dispatch(map[string]any{"repo": ghRepo, "workflow": "ci.yml", "ref": "main", "inputs": many}); resp.StatusCode != 400 {
		t.Errorf("26 inputs: status %d", resp.StatusCode)
	}
	if g.calls("dispatches") != before {
		t.Errorf("a refused request still reached GitHub")
	}
	if !strings.Contains(string(admin.expect(200, "GET", "/v1/audit", nil)), "is not allowed") {
		t.Error("the refusal is not in the audit log")
	}
}

func TestActionsAreForDevelopersAndLimitedPerMinute(t *testing.T) {
	g := newActionsGH(t)
	h := newHarness(t, &executor.Fake{})
	admin := connectActions(t, h, g)
	admin.expect(201, "POST", "/v1/users", map[string]any{"username": "vic", "password": pw, "role": "viewer"})
	viewer := h.login("vic")
	for _, c := range [][2]string{{"GET", "/v1/actions/repos"}, {"GET", "/v1/actions/runs?repo=" + ghRepo}, {"POST", "/v1/actions/dispatch"}, {"GET", "/v1/actions/jobs/1/logs?repo=" + ghRepo}} {
		viewer.expect(403, c[0], c[1], map[string]any{})
	}
	for i := 0; i < 20; i++ {
		admin.expect(200, "POST", "/v1/actions/runs/901/rerun", map[string]any{"repo": ghRepo})
	}
	admin.expect(429, "POST", "/v1/actions/runs/901/cancel", map[string]any{"repo": ghRepo})
}

func TestActionsCancelAndRerun(t *testing.T) {
	g := newActionsGH(t)
	h := newHarness(t, &executor.Fake{})
	admin := connectActions(t, h, g)
	admin.expect(200, "POST", "/v1/actions/runs/902/cancel", map[string]any{"repo": ghRepo})
	admin.expect(200, "POST", "/v1/actions/runs/901/rerun", map[string]any{"repo": ghRepo, "failed_only": true})
	if g.calls("POST /repos/acme/widgets/actions/runs/902/cancel") != 1 || g.calls("runs/901/rerun-failed-jobs") != 1 {
		t.Errorf("hits = %v", g.hits)
	}
	admin.expect(400, "POST", "/v1/actions/runs/9x/cancel", map[string]any{"repo": ghRepo})
	admin.expect(403, "POST", "/v1/actions/runs/901/cancel", map[string]any{"repo": "evil/repo"})
}

func TestActionsLogsKeepTheTokenAwayFromTheDownloadAddress(t *testing.T) {
	g := newActionsGH(t)
	g.log = "ok\n" + strings.Repeat("x", logTailBytes+500) + "\nlast line \xff\xfe\n"
	h := newHarness(t, &executor.Fake{})
	admin := connectActions(t, h, g)

	var out struct {
		Text      string
		Truncated bool
	}
	json.Unmarshal(admin.expect(200, "GET", "/v1/actions/jobs/5001/logs?repo="+ghRepo, nil), &out)
	if !out.Truncated || !strings.Contains(out.Text, "last line") || strings.HasPrefix(out.Text, "ok") {
		t.Errorf("expected the tail of the log, got %d bytes truncated=%v", len(out.Text), out.Truncated)
	}
	if strings.Contains(out.Text, "\xff") {
		t.Error("invalid UTF-8 was passed through")
	}
	if g.auth["GET /repos/acme/widgets/actions/jobs/5001/logs"] != "Bearer "+ghToken {
		t.Error("the log request to GitHub carried no token")
	}
	g.mu.Lock()
	dl := append([]string(nil), g.dlAuth...)
	g.mu.Unlock()
	if len(dl) != 1 || dl[0] != "" {
		t.Errorf("the download address received credentials: %q", dl)
	}
}

func TestActionsLogsRefuseAddressesThatAreNotWebLinks(t *testing.T) {
	g := newActionsGH(t)
	h := newHarness(t, &executor.Fake{})
	admin := connectActions(t, h, g)
	for _, loc := range []string{"file:///etc/passwd", "ftp://example.com/x", "https://user:pw@example.com/x", "//", "gopher://x/"} {
		g.mu.Lock()
		g.redir = loc
		g.mu.Unlock()
		resp, body := admin.do("GET", "/v1/actions/jobs/5001/logs?repo="+ghRepo, nil)
		if resp.StatusCode != 502 || !strings.Contains(string(body), "not a web link") {
			t.Errorf("%s: %d %s", loc, resp.StatusCode, body)
		}
	}
	// And the pure check: a bare IP or plain http is refused when the API is a real https host.
	for _, loc := range []string{"https://169.254.169.254/latest", "http://logs.example.com/x"} {
		if _, err := checkLogURL("https://api.github.com", loc); err == nil {
			t.Errorf("%s was accepted", loc)
		}
	}
	if _, err := checkLogURL("https://api.github.com", "https://productionresultssa1.blob.core.windows.net/x?sig=1"); err != nil {
		t.Errorf("a normal download address was refused: %v", err)
	}
}

func TestActionsExplainAMissingPermissionAndAnUnconnectedGitHub(t *testing.T) {
	g := newActionsGH(t)
	h := newHarness(t, &executor.Fake{})
	admin := h.adminBrowser("ada")
	admin.expect(409, "GET", "/v1/actions/workflows?repo="+ghRepo, nil)
	var empty struct{ Connected bool }
	json.Unmarshal(admin.expect(200, "GET", "/v1/actions/repos", nil), &empty)
	if empty.Connected {
		t.Error("reported connected before GitHub was set up")
	}

	admin.expect(200, "PUT", "/v1/integrations/github", map[string]any{
		"enabled": true, "api_base": g.srv.URL, "rules": []map[string]any{{"repo": ghRepo, "branches": []string{"main"}}},
		"token": ghToken, "generate_webhook_secret": true,
	})
	g.mu.Lock()
	g.status["/repos/acme/widgets/actions/workflows"] = 403
	g.mu.Unlock()
	body := admin.expect(502, "GET", "/v1/actions/workflows?repo="+ghRepo, nil)
	if !strings.Contains(string(body), "Actions permission") {
		t.Errorf("message = %s", body)
	}
}

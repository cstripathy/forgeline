package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cstripathy/forgeline/internal/executor"
	"github.com/cstripathy/forgeline/internal/store"
)

const (
	ghSecret = "webhook-secret-0123456789"
	ghToken  = "github-token-for-tests"
	ghRepo   = "acme/widgets"
)

var (
	sha1st = strings.Repeat("a", 40)
	sha2nd = strings.Repeat("b", 40)
	sha3rd = strings.Repeat("c", 40)
)

type statusPost struct {
	Repo, SHA                              string
	State, Description, Context, TargetURL string
}

// fakeGitHub serves the two endpoints Forgeline uses: tarball download and commit status.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	tarballs map[string][]byte // "owner/repo@sha" -> tar.gz
	statuses []statusPost
	gate     chan struct{} // if set, tarball requests wait for it to close
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{t: t, tarballs: map[string][]byte{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+ghToken {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // repos owner repo kind sha
	if len(parts) != 5 || parts[0] != "repos" {
		http.NotFound(w, r)
		return
	}
	repo, kind, sha := parts[1]+"/"+parts[2], parts[3], parts[4]
	switch {
	case r.Method == http.MethodGet && kind == "tarball":
		g.mu.Lock()
		gate := g.gate
		data, ok := g.tarballs[repo+"@"+sha]
		g.mu.Unlock()
		if gate != nil {
			<-gate
		}
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		w.Write(data)
	case r.Method == http.MethodPost && kind == "statuses":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		g.statuses = append(g.statuses, statusPost{repo, sha, body["state"], body["description"], body["context"], body["target_url"]})
		g.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("{}"))
	default:
		http.NotFound(w, r)
	}
}

// addCommit makes a GitHub-style tarball available: pax global header, a top-level directory
// named after the commit, the given files, and a symlink.
func (g *fakeGitHub) addCommit(repo, sha string, files map[string]string) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	top := strings.ReplaceAll(repo, "/", "-") + "-" + sha[:7] + "/"
	must := func(err error) {
		if err != nil {
			g.t.Fatal(err)
		}
	}
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header",
		PAXRecords: map[string]string{"comment": sha}}))
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: top, Mode: 0o755}))
	for name, content := range files {
		must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: top + name, Mode: 0o644, Size: int64(len(content))}))
		_, err := tw.Write([]byte(content))
		must(err)
	}
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: top + "link", Linkname: "src.txt"}))
	must(tw.Close())
	must(gz.Close())
	g.mu.Lock()
	g.tarballs[repo+"@"+sha] = buf.Bytes()
	g.mu.Unlock()
}

func (g *fakeGitHub) posted() []statusPost {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]statusPost(nil), g.statuses...)
}

// states returns the states posted for sha, in order.
func (g *fakeGitHub) states(sha string) []string {
	var out []string
	for _, p := range g.posted() {
		if p.SHA == sha {
			out = append(out, p.State)
		}
	}
	return out
}

func (g *fakeGitHub) waitStates(h *harness, sha string, want ...string) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("statuses %v for %s (have %v)", want, sha[:7], g.states(sha)), func() bool {
		got := g.states(sha)
		return strings.Join(got, ",") == strings.Join(want, ",")
	})
}

func ghHarness(t *testing.T, gh *fakeGitHub, fake *executor.Fake, mutate ...func(*GitHubConfig)) *harness {
	t.Helper()
	return newHarness(t, fake, func(c *Config) {
		c.GitHub = &GitHubConfig{
			WebhookSecret: ghSecret, Token: ghToken, APIBase: gh.srv.URL, PublicURL: "https://ci.example.com/",
			Allow: []RepoRule{{Repo: ghRepo, Branches: []string{"main", "release/*"}}},
		}
		for _, m := range mutate {
			m(c.GitHub)
		}
	})
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (h *harness) webhook(event, delivery string, payload any) (*http.Response, map[string]string) {
	h.t.Helper()
	body, _ := json.Marshal(payload)
	return h.webhookRaw(event, delivery, body, sign(ghSecret, body))
}

func (h *harness) webhookRaw(event, delivery string, body []byte, signature string) (*http.Response, map[string]string) {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.http.URL+"/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]string
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	return resp, out
}

func pushEvent(repo, ref, sha string) map[string]any {
	return map[string]any{"ref": ref, "after": sha, "deleted": false, "repository": map[string]any{"full_name": repo}}
}

func prEvent(action, baseRepo, baseRef, headRepo, sha string, n int) map[string]any {
	var head any
	if headRepo != "" {
		head = map[string]any{"full_name": headRepo}
	}
	return map[string]any{"action": action, "number": n, "pull_request": map[string]any{
		"head": map[string]any{"sha": sha, "repo": head},
		"base": map[string]any{"ref": baseRef, "repo": map[string]any{"full_name": baseRepo}},
	}}
}

func (h *harness) runs() []store.Run {
	h.t.Helper()
	runs, err := h.store.ListRuns(context.Background(), store.ListOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	return runs
}

func (h *harness) noRunsFor(why string) {
	h.t.Helper()
	time.Sleep(100 * time.Millisecond) // let any wrongly started checkout finish
	if n := len(h.runs()); n != 0 {
		h.t.Fatalf("%s: %d run(s) were created, want none", why, n)
	}
}

const ghPipeline = twoSteps

func TestGitHubPushRunsAndReportsStatus(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addCommit(ghRepo, sha1st, map[string]string{"forgeline.yaml": ghPipeline, "src.txt": "hello from the repo"})

	var mu sync.Mutex
	var seen []string
	h := ghHarness(t, gh, &executor.Fake{Handler: func(ctx context.Context, s executor.StepRun, logs io.Writer) (executor.Result, error) {
		b, err := os.ReadFile(filepath.Join(s.Workspace, "src.txt"))
		_, linkErr := os.Lstat(filepath.Join(s.Workspace, "link"))
		mu.Lock()
		seen = append(seen, fmt.Sprintf("%s:%s:%v:symlink-present=%v", s.ID, b, err, linkErr == nil))
		mu.Unlock()
		return executor.Result{}, nil
	}})

	resp, out := h.webhook("push", "d1", pushEvent(ghRepo, "refs/heads/main", sha1st))
	if resp.StatusCode != http.StatusAccepted || out["status"] != "accepted" {
		t.Fatalf("webhook = %d %v, want 202 accepted", resp.StatusCode, out)
	}
	gh.waitStates(h, sha1st, "pending", "success")

	runs := h.runs()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	r := runs[0]
	if r.Trigger != store.TriggerGitHub || r.SourceRepo != ghRepo || r.SourceSHA != sha1st || r.SourceRef != "refs/heads/main" || r.Status != store.RunSucceeded {
		t.Errorf("run = %+v", r)
	}
	if r.PipelineYAML != ghPipeline {
		t.Errorf("stored pipeline text differs from the committed file:\n%s", r.PipelineYAML)
	}
	for _, s := range gh.posted() {
		if s.Context != "forgeline" || s.TargetURL != "https://ci.example.com/#/runs/"+r.ID {
			t.Errorf("status = %+v, want context forgeline and a link to the run", s)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := "hello from the repo:<nil>:symlink-present=false"
	for _, line := range seen {
		if !strings.HasSuffix(line, want) {
			t.Errorf("step saw %q, want it to end with %q", line, want)
		}
	}
	if len(seen) != 2 {
		t.Errorf("steps run = %v, want 2", seen)
	}
}

func TestGitHubFailedRunPostsFailure(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addCommit(ghRepo, sha1st, map[string]string{"forgeline.yaml": ghPipeline})
	h := ghHarness(t, gh, &executor.Fake{Handler: func(ctx context.Context, s executor.StepRun, logs io.Writer) (executor.Result, error) {
		return executor.Result{ExitCode: 2}, nil
	}})
	h.webhook("push", "d1", pushEvent(ghRepo, "refs/heads/main", sha1st))
	gh.waitStates(h, sha1st, "pending", "failure")
	last := gh.posted()[1]
	if !strings.Contains(last.Description, "build") {
		t.Errorf("failure description %q should name the failed step", last.Description)
	}
}

func TestGitHubRejectsBadSignatures(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addCommit(ghRepo, sha1st, map[string]string{"forgeline.yaml": ghPipeline})
	h := ghHarness(t, gh, &executor.Fake{})
	body, _ := json.Marshal(pushEvent(ghRepo, "refs/heads/main", sha1st))

	cases := map[string]string{
		"missing":           "",
		"wrong secret":      sign("another-secret-0123456789", body),
		"no sha256 prefix":  strings.TrimPrefix(sign(ghSecret, body), "sha256="),
		"sha1 style prefix": "sha1=" + strings.TrimPrefix(sign(ghSecret, body), "sha256="),
		"not hex":           "sha256=zzzz",
		"body tampered":     sign(ghSecret, append([]byte("x"), body...)),
	}
	for name, sig := range cases {
		t.Run(name, func(t *testing.T) {
			resp, _ := h.webhookRaw("push", "d-"+name, body, sig)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
	h.noRunsFor("bad signatures")
	if n := len(gh.posted()); n != 0 {
		t.Errorf("unauthenticated requests caused %d GitHub calls", n)
	}

	// A malformed body with a valid signature is a client error, not a crash.
	bad := []byte("{not json")
	if resp, _ := h.webhookRaw("push", "d-bad", bad, sign(ghSecret, bad)); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed signed body = %d, want 400", resp.StatusCode)
	}
}

func TestGitHubPingAndUnhandledEvents(t *testing.T) {
	gh := newFakeGitHub(t)
	h := ghHarness(t, gh, &executor.Fake{})
	if resp, out := h.webhook("ping", "p1", map[string]any{"zen": "x"}); resp.StatusCode != 200 || out["status"] != "pong" {
		t.Errorf("ping = %d %v", resp.StatusCode, out)
	}
	resp, out := h.webhook("issues", "i1", map[string]any{"action": "opened"})
	if resp.StatusCode != 200 || out["status"] != "ignored" {
		t.Errorf("issues event = %d %v, want 200 ignored", resp.StatusCode, out)
	}
	h.noRunsFor("unhandled events")
}

func TestGitHubIgnoresEventsThatMayNotTrigger(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addCommit(ghRepo, sha1st, map[string]string{"forgeline.yaml": ghPipeline})
	gh.addCommit("evil/fork", sha1st, map[string]string{"forgeline.yaml": ghPipeline})
	h := ghHarness(t, gh, &executor.Fake{})

	deleted := pushEvent(ghRepo, "refs/heads/main", strings.Repeat("0", 40))
	deleted["deleted"] = true
	tests := []struct {
		name, event string
		payload     any
		reason      string
	}{
		{"repo not on allow list", "push", pushEvent("evil/fork", "refs/heads/main", sha1st), "allow list"},
		{"branch not on allow list", "push", pushEvent(ghRepo, "refs/heads/feature", sha1st), "allow list"},
		{"tag push", "push", pushEvent(ghRepo, "refs/tags/v1", sha1st), "not a branch"},
		{"branch deleted", "push", deleted, "deleted"},
		{"pr from a fork", "pull_request", prEvent("opened", ghRepo, "main", "someone/widgets", sha1st, 7), "fork"},
		{"pr from a deleted fork", "pull_request", prEvent("opened", ghRepo, "main", "", sha1st, 7), "fork"},
		{"pr closed", "pull_request", prEvent("closed", ghRepo, "main", ghRepo, sha1st, 7), "does not start"},
		{"pr into a branch not allowed", "pull_request", prEvent("opened", ghRepo, "develop", ghRepo, sha1st, 7), "allow list"},
		{"pr in a repo not allowed", "pull_request", prEvent("opened", "evil/fork", "main", "evil/fork", sha1st, 7), "allow list"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, out := h.webhook(tt.event, fmt.Sprintf("ig-%d", i), tt.payload)
			if resp.StatusCode != 200 || out["status"] != "ignored" || !strings.Contains(out["reason"], tt.reason) {
				t.Errorf("= %d %v, want 200 ignored mentioning %q", resp.StatusCode, out, tt.reason)
			}
		})
	}
	h.noRunsFor("events that may not trigger")
	if n := len(gh.posted()); n != 0 {
		t.Errorf("ignored events caused %d status posts", n)
	}
}

func TestGitHubRepoAndBranchRulesAreExactAndCaseInsensitiveForRepo(t *testing.T) {
	c := &GitHubConfig{Allow: []RepoRule{{Repo: "Acme/Widgets", Branches: []string{"main", "release/*"}}}}
	for _, tt := range []struct {
		repo, branch string
		want         bool
	}{
		{"acme/widgets", "main", true},
		{"ACME/WIDGETS", "release/1.2", true},
		{"acme/widgets", "release/1/2", false}, // * does not cross a slash
		{"acme/widgets", "mainline", false},
		{"acme/widgets-extra", "main", false},
		{"acme/other", "main", false},
	} {
		if got := c.allowed(tt.repo, tt.branch); got != tt.want {
			t.Errorf("allowed(%q, %q) = %v, want %v", tt.repo, tt.branch, got, tt.want)
		}
	}
}

func TestGitHubSameRepoPullRequestRuns(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addCommit(ghRepo, sha2nd, map[string]string{"forgeline.yaml": ghPipeline})
	h := ghHarness(t, gh, &executor.Fake{})
	resp, _ := h.webhook("pull_request", "pr1", prEvent("synchronize", ghRepo, "main", ghRepo, sha2nd, 42))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	gh.waitStates(h, sha2nd, "pending", "success")
	if r := h.runs()[0]; r.SourceRef != "refs/pull/42/head" || r.SourceSHA != sha2nd {
		t.Errorf("run source = %s %s", r.SourceRef, r.SourceSHA)
	}
}

func TestGitHubCheckoutProblemsAreReportedAsErrorStatus(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addCommit(ghRepo, sha2nd, map[string]string{"README.md": "no pipeline here"})
	gh.addCommit(ghRepo, sha3rd, map[string]string{"forgeline.yaml": "version: v2\nname: BAD\nsteps: []\n"})
	h := ghHarness(t, gh, &executor.Fake{})

	// sha1st has no tarball at all: GitHub answers 404.
	for i, tt := range []struct{ sha, descr string }{
		{sha1st, "Could not download the commit"},
		{sha2nd, "No forgeline.yaml in this commit"},
		{sha3rd, "Invalid pipeline"},
	} {
		resp, _ := h.webhook("push", fmt.Sprintf("co-%d", i), pushEvent(ghRepo, "refs/heads/main", tt.sha))
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%s: status = %d, want 202 (the problem is reported through the commit status)", tt.descr, resp.StatusCode)
		}
		gh.waitStates(h, tt.sha, "error")
		var got statusPost
		for _, p := range gh.posted() {
			if p.SHA == tt.sha {
				got = p
			}
		}
		if !strings.Contains(got.Description, tt.descr) {
			t.Errorf("description = %q, want it to contain %q", got.Description, tt.descr)
		}
		if got.TargetURL != "" {
			t.Errorf("an error with no run should carry no link, got %q", got.TargetURL)
		}
	}
	h.noRunsFor("checkout problems")
	entries, _ := os.ReadDir(filepath.Join(h.srv.cfg.DataDir, "tmp"))
	if len(entries) != 0 {
		t.Errorf("scratch space not cleaned up after failures: %v", entries)
	}
}

func TestGitHubRedeliveryAfterFailedCheckoutIsProcessed(t *testing.T) {
	gh := newFakeGitHub(t)
	h := ghHarness(t, gh, &executor.Fake{})
	ev := pushEvent(ghRepo, "refs/heads/main", sha1st)

	h.webhook("push", "same-delivery", ev)
	gh.waitStates(h, sha1st, "error")

	gh.addCommit(ghRepo, sha1st, map[string]string{"forgeline.yaml": ghPipeline})
	resp, out := h.webhook("push", "same-delivery", ev)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("redelivery after a failure = %d %v, want 202", resp.StatusCode, out)
	}
	gh.waitStates(h, sha1st, "error", "pending", "success")
}

func TestGitHubDuplicateDeliveryStartsOneRun(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addCommit(ghRepo, sha1st, map[string]string{"forgeline.yaml": ghPipeline})
	h := ghHarness(t, gh, &executor.Fake{})
	ev := pushEvent(ghRepo, "refs/heads/main", sha1st)

	h.webhook("push", "dup", ev)
	resp, out := h.webhook("push", "dup", ev)
	if resp.StatusCode != 200 || out["reason"] != "duplicate delivery" {
		t.Errorf("second delivery = %d %v, want ignored as a duplicate", resp.StatusCode, out)
	}
	gh.waitStates(h, sha1st, "pending", "success")
	if n := len(h.runs()); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}
}

func TestGitHubLimitsConcurrentCheckouts(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addCommit(ghRepo, sha1st, map[string]string{"forgeline.yaml": ghPipeline})
	gh.addCommit(ghRepo, sha2nd, map[string]string{"forgeline.yaml": ghPipeline})
	gh.gate = make(chan struct{})
	h := ghHarness(t, gh, &executor.Fake{}, func(c *GitHubConfig) { c.MaxCheckouts = 1 })

	if resp, _ := h.webhook("push", "c1", pushEvent(ghRepo, "refs/heads/main", sha1st)); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first = %d, want 202", resp.StatusCode)
	}
	resp, _ := h.webhook("push", "c2", pushEvent(ghRepo, "refs/heads/main", sha2nd))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Errorf("second while the first downloads = %d (Retry-After %q), want 503 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	close(gh.gate)
	gh.waitStates(h, sha1st, "pending", "success")
	// The rejected delivery can be redelivered once there is room.
	if resp, _ := h.webhook("push", "c2", pushEvent(ghRepo, "refs/heads/main", sha2nd)); resp.StatusCode != http.StatusAccepted {
		t.Errorf("redelivery of the rejected event = %d, want 202", resp.StatusCode)
	}
}

func TestGitHubCancelingAQueuedRunPostsAStatus(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addCommit(ghRepo, sha1st, map[string]string{"forgeline.yaml": ghPipeline})
	gh.addCommit(ghRepo, sha2nd, map[string]string{"forgeline.yaml": ghPipeline})
	started := make(chan string, 4)
	h := ghHarness(t, gh, blockingFake(started)) // MaxRuns is 1: the second run stays queued

	h.webhook("push", "q1", pushEvent(ghRepo, "refs/heads/main", sha1st))
	<-started
	h.webhook("push", "q2", pushEvent(ghRepo, "refs/heads/main", sha2nd))
	gh.waitStates(h, sha2nd, "pending")

	var queued store.Run
	for _, r := range h.runs() {
		if r.SourceSHA == sha2nd {
			queued = r
		}
	}
	if resp, _ := cancel(h, queued.ID); resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel = %d", resp.StatusCode)
	}
	gh.waitStates(h, sha2nd, "pending", "error")
	if got := gh.posted(); !strings.Contains(got[len(got)-1].Description, "canceled") {
		t.Errorf("description = %q", got[len(got)-1].Description)
	}
}

func TestGitHubRestartPostsStatusForInterruptedRuns(t *testing.T) {
	gh := newFakeGitHub(t)
	st := store.NewMemory()
	run, err := st.CreateRun(context.Background(), store.Run{PipelineName: "demo", PipelineYAML: twoSteps,
		Trigger: store.TriggerGitHub, SourceRepo: ghRepo, SourceSHA: sha1st, SourceRef: "refs/heads/main"}, []string{"build", "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.ClaimQueuedRun(context.Background()); !ok || err != nil { // now "running", as after a crash
		t.Fatalf("claim: %v %v", ok, err)
	}

	srv, err := New(Config{Store: st, Executor: &executor.Fake{}, Token: testToken, DataDir: t.TempDir(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		GitHub: &GitHubConfig{WebhookSecret: ghSecret, Token: ghToken, APIBase: gh.srv.URL,
			Allow: []RepoRule{{Repo: ghRepo, Branches: []string{"main"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	h := &harness{t: t}
	h.waitFor("a status for the interrupted run", func() bool { return len(gh.states(sha1st)) == 1 })
	p := gh.posted()[0]
	if p.State != "failure" || !strings.Contains(p.Description, "server restarted") {
		t.Errorf("status = %+v, want failure mentioning the restart", p)
	}
	if got, _, _ := st.GetRun(context.Background(), run.ID); got.Status != store.RunFailed {
		t.Errorf("run status = %s, want failed", got.Status)
	}
}

func TestGitHubEndpointIsAbsentWhenNotConfigured(t *testing.T) {
	h := newHarness(t, &executor.Fake{})
	resp, _ := h.webhookRaw("push", "x", []byte("{}"), "sha256=00")
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 404 when GitHub is not configured", resp.StatusCode)
	}
}

func TestGitHubConfigValidation(t *testing.T) {
	good := func() GitHubConfig {
		return GitHubConfig{WebhookSecret: ghSecret, Token: ghToken, Allow: []RepoRule{{Repo: ghRepo, Branches: []string{"main"}}}}
	}
	tests := []struct {
		name   string
		mutate func(*GitHubConfig)
		want   string
	}{
		{"short secret", func(c *GitHubConfig) { c.WebhookSecret = "short" }, "webhook secret"},
		{"no token", func(c *GitHubConfig) { c.Token = "" }, "token is required"},
		{"empty allow list", func(c *GitHubConfig) { c.Allow = nil }, "Allow is empty"},
		{"bad repo name", func(c *GitHubConfig) { c.Allow[0].Repo = "no-slash" }, "owner/name"},
		{"repo with path tricks", func(c *GitHubConfig) { c.Allow[0].Repo = "a/b/../c" }, "owner/name"},
		{"no branches", func(c *GitHubConfig) { c.Allow[0].Branches = nil }, "at least one branch"},
		{"empty branch", func(c *GitHubConfig) { c.Allow[0].Branches = []string{""} }, "bad branch pattern"},
		{"bad branch pattern", func(c *GitHubConfig) { c.Allow[0].Branches = []string{"[a"} }, "bad branch pattern"},
		{"plain http api", func(c *GitHubConfig) { c.APIBase = "http://github.example.com" }, "plain http"},
		{"api not a url", func(c *GitHubConfig) { c.APIBase = "ftp://x" }, "not a valid"},
		{"bad public url", func(c *GitHubConfig) { c.PublicURL = "ci.example.com" }, "public URL"},
		{"absolute pipeline file", func(c *GitHubConfig) { c.PipelineFile = "/etc/passwd" }, "relative path"},
		{"pipeline file escapes", func(c *GitHubConfig) { c.PipelineFile = "../x.yaml" }, "relative path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := good()
			c.Allow = append([]RepoRule(nil), c.Allow...)
			tt.mutate(&c)
			_, err := New(Config{Store: store.NewMemory(), Executor: &executor.Fake{}, Token: testToken, DataDir: t.TempDir(), GitHub: &c})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("New() error = %v, want it to contain %q", err, tt.want)
			}
		})
	}

	c := good()
	c.APIBase = "http://127.0.0.1:9999"
	if _, err := New(Config{Store: store.NewMemory(), Executor: &executor.Fake{}, Token: testToken, DataDir: t.TempDir(), GitHub: &c}); err != nil {
		t.Errorf("loopback http API base rejected: %v", err)
	}
	if c.PipelineFile != "" {
		t.Error("New changed the caller's GitHubConfig")
	}
}

func TestExtractRepoTarGz(t *testing.T) {
	gh := newFakeGitHub(t)
	files := map[string]string{"forgeline.yaml": ghPipeline, "dir/deep/file.txt": "deep", "src.txt": "x"}
	gh.addCommit(ghRepo, sha1st, files)
	data := gh.tarballs[ghRepo+"@"+sha1st]

	dst := t.TempDir()
	skipped, err := ExtractRepoTarGz(bytes.NewReader(data), dst, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the symlink)", skipped)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); err == nil {
		t.Error("symlink was extracted")
	}
	// The strict upload extractor still refuses a repo tarball: it has a pax header and a symlink.
	if err := ExtractTarGz(bytes.NewReader(data), t.TempDir(), 1<<20); err == nil {
		t.Error("ExtractTarGz accepted a tarball with a symlink")
	}

	if _, err := ExtractRepoTarGz(bytes.NewReader(data), t.TempDir(), 3); err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Errorf("size limit error = %v", err)
	}
}

func TestExtractRepoTarGzRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "top/../../escape.txt", Mode: 0o644, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	parent := t.TempDir()
	dst := filepath.Join(parent, "ws")
	os.Mkdir(dst, 0o755)
	if _, err := ExtractRepoTarGz(&buf, dst, 1<<20); err == nil {
		t.Error("path traversal was accepted")
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
		t.Error("a file escaped the workspace")
	}
}

package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/forgelinehq/forgeline/internal/pipeline"
	"github.com/forgelinehq/forgeline/internal/store"
)

// GitHubConfig turns on the GitHub webhook trigger and commit status reporting. See
// docs/design/003-control-plane.md, "GitHub webhook".
//
// Trust limit until milestone M3: a run executes the repository's own pipeline with no
// sandbox beyond container defaults, so Allow must list only repositories and branches whose
// writers you would trust with a shell on this host. Pull requests from forks are always
// ignored.
type GitHubConfig struct {
	// WebhookSecret is the secret configured on the GitHub webhook. At least 16 characters.
	WebhookSecret string
	// Token is a fine-grained personal access token with read access to contents and
	// write access to commit statuses on the allowed repositories.
	Token string
	// TokenFunc, if set, supplies the token for each request (a GitHub App's short-lived
	// installation token) and replaces Token.
	TokenFunc func(ctx context.Context) (string, error)
	// Allow lists the repositories and branches that may trigger runs. Required.
	Allow []RepoRule
	// APIBase is the GitHub API root. Empty means https://api.github.com. Plain http is
	// accepted only for loopback addresses, because the token travels in every request.
	APIBase string
	// PublicURL is where users reach this server, used for the link on commit statuses.
	// Optional; without it statuses carry no link.
	PublicURL string
	// PipelineFile is the pipeline's path in the repository. Empty means forgeline.yaml.
	PipelineFile string
	// MaxCheckouts bounds repository downloads in flight. Zero means 4.
	MaxCheckouts int
	// HTTPClient is used for GitHub API calls. Nil means a client with a 60 second timeout.
	HTTPClient *http.Client
}

// RepoRule allows one repository to trigger runs from some branches.
type RepoRule struct {
	// Repo is "owner/name", compared case-insensitively.
	Repo string
	// Branches are names or path.Match patterns such as "release/*". A "*" does not cross a
	// slash. At least one is required; there is no allow-everything shortcut.
	Branches []string
}

const (
	statusContext     = "forgeline"
	maxWebhookBody    = 5 << 20
	maxStatusDescrLen = 140
	checkoutTimeout   = 2 * time.Minute
	statusTimeout     = 15 * time.Second
	maxSeenDeliveries = 1024
)

var (
	repoNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	shaRE      = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	zeroSHARE  = regexp.MustCompile(`^0+$`)
)

// validateGitHub checks cfg and fills defaults.
func validateGitHub(c *GitHubConfig) error {
	switch {
	case len(c.WebhookSecret) < MinTokenLength:
		return fmt.Errorf("github: webhook secret must be at least %d characters", MinTokenLength)
	case c.Token == "" && c.TokenFunc == nil:
		return errors.New("github: a token is required to download commits and post statuses")
	case len(c.Allow) == 0:
		return errors.New("github: Allow is empty; list at least one repository and its branches")
	}
	for i, rule := range c.Allow {
		if !repoNameRE.MatchString(rule.Repo) {
			return fmt.Errorf("github: allow[%d]: repository %q must look like owner/name", i, rule.Repo)
		}
		if len(rule.Branches) == 0 {
			return fmt.Errorf("github: allow[%d] (%s): list at least one branch", i, rule.Repo)
		}
		for _, b := range rule.Branches {
			if _, err := path.Match(b, ""); err != nil || b == "" {
				return fmt.Errorf("github: allow[%d] (%s): bad branch pattern %q", i, rule.Repo, b)
			}
		}
	}
	if c.APIBase == "" {
		c.APIBase = "https://api.github.com"
	}
	u, err := url.Parse(c.APIBase)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("github: API base %q is not a valid http(s) URL", c.APIBase)
	}
	if u.Scheme == "http" {
		if host := u.Hostname(); host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
			return fmt.Errorf("github: API base %q uses plain http; the token would be sent unencrypted", c.APIBase)
		}
	}
	c.APIBase = strings.TrimRight(c.APIBase, "/")
	if c.PublicURL != "" {
		pu, err := url.Parse(c.PublicURL)
		if err != nil || pu.Host == "" || (pu.Scheme != "https" && pu.Scheme != "http") {
			return fmt.Errorf("github: public URL %q is not a valid http(s) URL", c.PublicURL)
		}
		c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	}
	if c.PipelineFile == "" {
		c.PipelineFile = "forgeline.yaml"
	}
	if !filepath.IsLocal(c.PipelineFile) {
		return fmt.Errorf("github: pipeline file %q must be a relative path inside the repository", c.PipelineFile)
	}
	if c.MaxCheckouts <= 0 {
		c.MaxCheckouts = 4
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	return nil
}

// allowed reports whether a push or pull request to branch of repo may trigger a run.
func (c *GitHubConfig) allowed(repo, branch string) bool {
	for _, rule := range c.Allow {
		if !strings.EqualFold(rule.Repo, repo) {
			continue
		}
		for _, pat := range rule.Branches {
			if ok, _ := path.Match(pat, branch); ok {
				return true
			}
		}
	}
	return false
}

// validSignature checks GitHub's X-Hub-Signature-256 header against the raw body, in
// constant time.
func validSignature(secret, header string, body []byte) bool {
	digest, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(digest)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// ghTrigger is a verified, allowed event reduced to what a run needs.
type ghTrigger struct {
	Repo   string
	Ref    string // refs/heads/x, or refs/pull/N/head
	SHA    string
	Branch string // the branch that was pushed to, or a pull request's base branch
}

type pushPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type pullPayload struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Head struct {
			SHA  string `json:"sha"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
}

// triggerFromEvent turns a verified event into a trigger. When it returns nil the event is
// not ours to run, and reason says why, for the response and the logs.
func (c *GitHubConfig) triggerFromEvent(event string, body []byte) (t *ghTrigger, reason string, err error) {
	switch event {
	case "push":
		var p pushPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, "", fmt.Errorf("push payload: %w", err)
		}
		branch, ok := strings.CutPrefix(p.Ref, "refs/heads/")
		switch {
		case !ok:
			return nil, "not a branch push", nil
		case p.Deleted || zeroSHARE.MatchString(p.After):
			return nil, "branch deleted", nil
		case !shaRE.MatchString(p.After):
			return nil, "", fmt.Errorf("push payload: %q is not a commit SHA", p.After)
		case !c.allowed(p.Repository.FullName, branch):
			return nil, fmt.Sprintf("%s on branch %q is not on the allow list", p.Repository.FullName, branch), nil
		}
		return &ghTrigger{Repo: p.Repository.FullName, Ref: p.Ref, SHA: p.After, Branch: branch}, "", nil

	case "pull_request":
		var p pullPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, "", fmt.Errorf("pull_request payload: %w", err)
		}
		switch p.Action {
		case "opened", "synchronize", "reopened":
		default:
			return nil, fmt.Sprintf("pull request action %q does not start a run", p.Action), nil
		}
		pr := p.PullRequest
		base := pr.Base.Repo.FullName
		switch {
		case pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, base):
			return nil, "pull request from a fork; its code is untrusted", nil
		case !shaRE.MatchString(pr.Head.SHA):
			return nil, "", fmt.Errorf("pull_request payload: %q is not a commit SHA", pr.Head.SHA)
		case !c.allowed(base, pr.Base.Ref):
			return nil, fmt.Sprintf("%s pull request into %q is not on the allow list", base, pr.Base.Ref), nil
		}
		return &ghTrigger{Repo: base, Ref: fmt.Sprintf("refs/pull/%d/head", p.Number), SHA: pr.Head.SHA, Branch: pr.Base.Ref}, "", nil
	}
	return nil, fmt.Sprintf("event %q is not handled", event), nil
}

// deliveryLog remembers recent webhook delivery IDs so a redelivery that reaches us twice
// does not start two runs. It lives in memory, so a restart forgets it.
type deliveryLog struct {
	mu    sync.Mutex
	seen  map[string]bool
	order []string
}

// add records id and reports whether it was new.
func (d *deliveryLog) add(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen[id] {
		return false
	}
	if d.seen == nil {
		d.seen = make(map[string]bool)
	}
	d.seen[id] = true
	d.order = append(d.order, id)
	if len(d.order) > maxSeenDeliveries {
		delete(d.seen, d.order[0])
		d.order = d.order[1:]
	}
	return true
}

// forget lets the same delivery be processed again, which GitHub's "redeliver" button needs
// after a failed checkout.
func (d *deliveryLog) forget(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.seen, id)
}

// handleGitHubWebhook authenticates by signature only. The signature is checked before the
// payload is parsed, and nothing about an unsigned request is acted on.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	gh := s.gh()
	if gh == nil {
		writeError(w, http.StatusNotFound, "the GitHub integration is not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		s.badBody(w, err)
		return
	}
	if !validSignature(gh.WebhookSecret, r.Header.Get("X-Hub-Signature-256"), body) {
		s.log.Warn("github webhook: rejected a request with a missing or invalid signature", "remote", r.RemoteAddr)
		s.deliveryRing.add(DeliveryInfo{Event: r.Header.Get("X-GitHub-Event"), Outcome: "rejected", Detail: "missing or invalid signature"})
		writeError(w, http.StatusUnauthorized, "missing or invalid signature")
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		s.deliveryRing.add(DeliveryInfo{Event: "ping", Outcome: "ok", Detail: "GitHub can reach this server and the secret matches"})
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	}
	trig, reason, err := gh.triggerFromEvent(event, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if trig == nil {
		s.log.Info("github webhook: ignored", "event", event, "reason", reason)
		s.deliveryRing.add(DeliveryInfo{Event: event, Outcome: "ignored", Detail: reason})
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": reason})
		return
	}

	delivery := r.Header.Get("X-GitHub-Delivery")
	if delivery != "" && !s.deliveries.add(delivery) {
		s.deliveryRing.add(DeliveryInfo{ID: delivery, Event: event, Repo: trig.Repo, Ref: trig.Ref, SHA: trig.SHA, Outcome: "ignored", Detail: "duplicate delivery"})
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "duplicate delivery"})
		return
	}
	s.deliveryRing.add(DeliveryInfo{ID: delivery, Event: event, Repo: trig.Repo, Ref: trig.Ref, SHA: trig.SHA, Outcome: "accepted", Detail: "downloading the commit"})
	select {
	case s.checkouts <- struct{}{}:
	default:
		s.deliveryRing.update(delivery, "busy", "too many commits are being downloaded; redeliver in a moment", "")
		s.deliveries.forget(delivery)
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "too many commits are being downloaded; redeliver in a moment")
		return
	}

	// GitHub gives up after 10 seconds and a checkout can take longer, so answer now and
	// report the outcome through the commit status.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.checkouts }()
		if !s.startGitHubRun(*trig, delivery) {
			s.deliveries.forget(delivery)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// startGitHubRun downloads the commit, validates its pipeline, and queues a run. It
// reports whether a run was created. Every failure is also posted as a commit status
// of "error", because a webhook caller has already been answered.
func (s *Server) startGitHubRun(t ghTrigger, delivery string) bool {
	ctx, cancel := context.WithTimeout(s.baseContext(), checkoutTimeout)
	defer cancel()
	fail := func(descr string, err error) bool {
		s.log.Warn("github run not started", "repo", t.Repo, "sha", t.SHA, "why", descr, "err", err)
		s.deliveryRing.update(delivery, "failed", descr, "")
		s.postStatus(t.Repo, t.SHA, "error", descr, "")
		return false
	}
	gh := s.gh()
	if gh == nil {
		return fail("The GitHub integration was turned off", nil)
	}

	ws, err := os.MkdirTemp(filepath.Join(s.cfg.DataDir, "tmp"), "ws-")
	if err != nil {
		return fail("Forgeline could not prepare a workspace", err)
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(ws)
		}
	}()

	skipped, err := s.fetchCommit(ctx, t.Repo, t.SHA, ws)
	if err != nil {
		return fail("Could not download the commit: "+err.Error(), err)
	}
	if skipped > 0 {
		s.log.Info("github checkout skipped entries that are not files or directories", "repo", t.Repo, "sha", t.SHA, "count", skipped)
	}

	text, err := os.ReadFile(filepath.Join(ws, gh.PipelineFile))
	if err != nil {
		return fail(fmt.Sprintf("No %s in this commit", gh.PipelineFile), err)
	}
	p, err := pipeline.Load(bytes.NewReader(text))
	if err == nil {
		_, err = pipeline.Plan(p)
	}
	if err != nil {
		return fail("Invalid pipeline: "+firstLine(err.Error()), err)
	}

	stepIDs := make([]string, len(p.Steps))
	for i, st := range p.Steps {
		stepIDs[i] = st.ID
	}
	run, err := s.cfg.Store.CreateRun(ctx, store.Run{
		PipelineName: p.Name, PipelineYAML: string(text), Trigger: store.TriggerGitHub,
		SourceRepo: t.Repo, SourceRef: t.Ref, SourceSHA: t.SHA,
	}, stepIDs)
	if err != nil {
		return fail("Forgeline could not record the run", err)
	}
	if err := os.Rename(ws, s.workspace(run.ID)); err != nil {
		s.failQueued(run.ID, "could not place workspace")
		return fail("Forgeline could not place the workspace", err)
	}
	committed = true
	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.recordRunMeta(ctx, run.ID, runMeta{Actor: "github"})
	s.audit(ctx, Principal{Name: "github"}, "", "run.start", "run/"+run.ID, fmt.Sprintf("%s@%.7s (%s)", t.Repo, t.SHA, p.Name), true)
	s.deliveryRing.update(delivery, "run started", p.Name, run.ID)
	s.postStatus(t.Repo, t.SHA, "pending", "Run queued", run.ID)
	return true
}

// fetchCommit downloads the repository at sha through the tarball API and unpacks it into dst.
func (s *Server) fetchCommit(ctx context.Context, repo, sha, dst string) (skipped int, err error) {
	gh := s.gh()
	if gh == nil {
		return 0, errors.New("the GitHub integration was turned off")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gh.APIBase+"/repos/"+repo+"/tarball/"+sha, nil)
	if err != nil {
		return 0, err
	}
	if err := s.githubHeaders(req); err != nil {
		return 0, err
	}
	resp, err := gh.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return 0, fmt.Errorf("GitHub answered %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	// The same cap bounds the download and the unpacked size, so a hostile or huge
	// repository stops early.
	skipped, err = ExtractRepoTarGz(io.LimitReader(resp.Body, s.cfg.MaxSourceBytes), dst, s.cfg.MaxSourceBytes)
	if err != nil {
		return 0, fmt.Errorf("%w (limit %d bytes)", err, s.cfg.MaxSourceBytes)
	}
	return skipped, nil
}

func (s *Server) githubHeaders(req *http.Request) error {
	if gh := s.gh(); gh != nil {
		token := gh.Token
		if gh.TokenFunc != nil {
			t, err := gh.TokenFunc(req.Context())
			if err != nil {
				return err
			}
			token = t
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "forgeline")
	return nil
}

// postStatus reports a commit status. Failure is logged and never changes a run's result.
func (s *Server) postStatus(repo, sha, state, descr, runID string) {
	gh := s.gh()
	if gh == nil {
		return
	}
	body := map[string]string{
		"state": state, "context": statusContext, "description": truncate(descr, maxStatusDescrLen),
	}
	if runID != "" && gh.PublicURL != "" {
		body["target_url"] = gh.PublicURL + "/#/runs/" + runID
	}
	b, _ := json.Marshal(body)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.baseContext()), statusTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gh.APIBase+"/repos/"+repo+"/statuses/"+sha, bytes.NewReader(b))
	if err != nil {
		s.log.Warn("github status: building request", "err", err)
		return
	}
	if err := s.githubHeaders(req); err != nil {
		s.log.Warn("github status: getting a token", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := gh.HTTPClient.Do(req)
	if err != nil {
		s.log.Warn("github status: request failed", "repo", repo, "sha", sha, "state", state, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		s.log.Warn("github status: not accepted", "repo", repo, "sha", sha, "state", state,
			"http_status", resp.StatusCode, "body", strings.TrimSpace(string(snippet)))
	}
}

// reportFinished posts the final commit status for a run that came from GitHub.
func (s *Server) reportFinished(run store.Run, status store.RunStatus, reason string) {
	if s.gh() == nil || run.Trigger != store.TriggerGitHub || run.SourceRepo == "" || run.SourceSHA == "" {
		return
	}
	state, descr := "error", "Run canceled"
	switch status {
	case store.RunSucceeded:
		state, descr = "success", "Pipeline succeeded"
	case store.RunFailed:
		state, descr = "failure", "Pipeline failed"
		if reason != "" {
			descr = reason
		}
	case store.RunCanceled:
		if reason != "" {
			descr = "Run canceled: " + reason
		}
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.postStatus(run.SourceRepo, run.SourceSHA, state, descr, run.ID)
	}()
}

func (s *Server) baseContext() context.Context {
	if s.baseCtx != nil {
		return s.baseCtx
	}
	return context.Background()
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if rest := strings.TrimSpace(strings.TrimPrefix(s, line)); rest != "" {
		// A validation error lists problems on following lines; show the first one.
		next, _, _ := strings.Cut(rest, "\n")
		return strings.TrimSpace(line) + " " + strings.TrimSpace(next)
	}
	return strings.TrimSpace(line)
}

// truncate shortens s to at most n runes, marking the cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

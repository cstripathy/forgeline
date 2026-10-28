package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/forgelinehq/forgeline/internal/docstore"
)

const (
	kindSettings    = "settings"
	idGitHub        = "github"
	ghTokenSecret   = "forgeline.github.token"
	ghWebhookSecret = "forgeline.github.webhook_secret"
)

// DeliveryInfo is one webhook delivery as the console shows it.
type DeliveryInfo struct {
	Time    time.Time `json:"time"`
	ID      string    `json:"id,omitempty"`
	Event   string    `json:"event"`
	Repo    string    `json:"repo,omitempty"`
	Ref     string    `json:"ref,omitempty"`
	SHA     string    `json:"sha,omitempty"`
	Outcome string    `json:"outcome"` // ok, ignored, rejected, accepted, run started, failed, busy
	Detail  string    `json:"detail,omitempty"`
	RunID   string    `json:"run_id,omitempty"`
}

// deliveryRing keeps the last few deliveries in memory. It is a diagnostic aid, not a record:
// the audit log holds the runs that were started.
type deliveryRing struct {
	mu    sync.Mutex
	items []DeliveryInfo // newest last
}

const maxRingItems = 30

func (d *deliveryRing) add(i DeliveryInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	i.Time = time.Now().UTC()
	d.items = append(d.items, i)
	if len(d.items) > maxRingItems {
		d.items = d.items[len(d.items)-maxRingItems:]
	}
}

// update changes the outcome of a delivery already in the ring.
func (d *deliveryRing) update(id, outcome, detail, runID string) {
	if id == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.items) - 1; i >= 0; i-- {
		if d.items[i].ID == id {
			d.items[i].Outcome, d.items[i].Detail = outcome, detail
			if runID != "" {
				d.items[i].RunID = runID
			}
			return
		}
	}
}

func (d *deliveryRing) list() []DeliveryInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]DeliveryInfo, len(d.items))
	for i, it := range d.items {
		out[len(d.items)-1-i] = it // newest first
	}
	return out
}

// gh returns the active GitHub settings, or nil when the integration is off.
func (s *Server) gh() *GitHubConfig { return s.ghCfg.Load() }

// githubSettings is what the console stores. The token and webhook secret are in the vault.
type githubSettings struct {
	Enabled      bool         `json:"enabled"`
	APIBase      string       `json:"api_base,omitempty"`
	PublicURL    string       `json:"public_url,omitempty"`
	PipelineFile string       `json:"pipeline_file,omitempty"`
	Rules        []githubRule `json:"rules"`
	// A GitHub App created from the console (design note 010). When AppID is set, the app's
	// installation token is used in place of a personal access token.
	AppID          int64     `json:"app_id,omitempty"`
	AppSlug        string    `json:"app_slug,omitempty"`
	AppURL         string    `json:"app_url,omitempty"`
	InstallationID int64     `json:"installation_id,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
	UpdatedBy      string    `json:"updated_by"`
	_              struct{}  `json:"-"`
}

type githubRule struct {
	Repo     string   `json:"repo"`
	Branches []string `json:"branches"`
}

func (st githubSettings) rules() []RepoRule {
	out := make([]RepoRule, len(st.Rules))
	for i, r := range st.Rules {
		out[i] = RepoRule{Repo: r.Repo, Branches: r.Branches}
	}
	return out
}

// buildGitHub assembles and validates a config from settings and the two secrets.
func buildGitHub(st githubSettings, token, webhook string, base *GitHubConfig, tokenFn func(context.Context) (string, error)) (*GitHubConfig, error) {
	c := &GitHubConfig{WebhookSecret: webhook, Token: token, TokenFunc: tokenFn, Allow: st.rules(), APIBase: st.APIBase,
		PublicURL: st.PublicURL, PipelineFile: st.PipelineFile}
	if base != nil { // keep test hooks such as a custom HTTP client
		c.HTTPClient, c.MaxCheckouts = base.HTTPClient, base.MaxCheckouts
	}
	if err := validateGitHub(c); err != nil {
		return nil, err
	}
	return c, nil
}

// reloadGitHub makes the active config match what the console stored. With no stored settings
// the command-line config, if any, stays in force.
func (s *Server) reloadGitHub(ctx context.Context) error {
	var st githubSettings
	err := s.docs.Get(ctx, kindSettings, idGitHub, &st)
	if errors.Is(err, docstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Enabled {
		s.ghCfg.Store(nil)
		s.ghSource.Store("console")
		return nil
	}
	token, _ := s.internalSecretValue(ctx, ghTokenSecret)
	webhook, _ := s.internalSecretValue(ctx, ghWebhookSecret)
	var tokenFn func(context.Context) (string, error)
	if st.AppID != 0 && st.InstallationID != 0 {
		token, tokenFn = "", s.installationTokenFunc(st)
	}
	c, err := buildGitHub(st, token, webhook, s.ghCfg.Load(), tokenFn)
	if err != nil {
		s.ghCfg.Store(nil)
		s.ghSource.Store("console")
		return err
	}
	s.ghCfg.Store(c)
	s.ghSource.Store("console")
	return nil
}

// GitHubJSON is the integration as the console shows it.
type GitHubJSON struct {
	Configured bool   `json:"configured"`
	Source     string `json:"source"` // "console", "flags" or "none"
	Enabled    bool   `json:"enabled"`
	// Active is true when webhooks are being accepted right now.
	Active           bool           `json:"active"`
	Problem          string         `json:"problem,omitempty"`
	APIBase          string         `json:"api_base"`
	PublicURL        string         `json:"public_url"`
	PipelineFile     string         `json:"pipeline_file"`
	Rules            []githubRule   `json:"rules"`
	TokenSet         bool           `json:"token_set"`
	WebhookSecretSet bool           `json:"webhook_secret_set"`
	WebhookURL       string         `json:"webhook_url"`
	UpdatedAt        *time.Time     `json:"updated_at,omitempty"`
	UpdatedBy        string         `json:"updated_by,omitempty"`
	Deliveries       []DeliveryInfo `json:"deliveries"`
	// App describes a GitHub App created from the console, if there is one.
	App *GHAppJSON `json:"app,omitempty"`
	// GeneratedWebhookSecret appears once, in the response that generated it.
	GeneratedWebhookSecret string `json:"generated_webhook_secret,omitempty"`
}

func (s *Server) githubView(ctx context.Context, r *http.Request) GitHubJSON {
	out := GitHubJSON{Source: "none", Rules: []githubRule{}, Deliveries: s.deliveryRing.list(), APIBase: "https://api.github.com", PipelineFile: "forgeline.yaml"}
	active := s.gh()
	out.Active = active != nil
	if src, _ := s.ghSource.Load().(string); src != "" {
		out.Source = src
	}
	var st githubSettings
	switch err := s.docs.Get(ctx, kindSettings, idGitHub, &st); {
	case err == nil:
		out.Source, out.Configured, out.Enabled = "console", true, st.Enabled
		if st.APIBase != "" {
			out.APIBase = st.APIBase
		}
		if st.PipelineFile != "" {
			out.PipelineFile = st.PipelineFile
		}
		out.PublicURL, out.Rules = st.PublicURL, st.Rules
		if st.AppID != 0 {
			out.App = &GHAppJSON{ID: st.AppID, Slug: st.AppSlug, URL: st.AppURL, InstallationID: st.InstallationID}
		}
		out.UpdatedAt, out.UpdatedBy = &st.UpdatedAt, st.UpdatedBy
		_, out.TokenSet = s.internalSecretValue(ctx, ghTokenSecret)
		_, out.WebhookSecretSet = s.internalSecretValue(ctx, ghWebhookSecret)
		if st.Enabled && active == nil && (s.secretUnreadable(ctx, ghTokenSecret) || s.secretUnreadable(ctx, ghWebhookSecret)) {
			out.Problem = "the stored token or webhook secret cannot be opened with this server's master key; set FORGELINE_MASTER_KEY to the key they were saved with, or save them again"
		} else if st.Enabled && active == nil {
			var fn func(context.Context) (string, error)
			if st.AppID != 0 && st.InstallationID != 0 {
				fn = s.installationTokenFunc(st)
			}
			if _, err := buildGitHub(st, mustSecret(s, ctx, ghTokenSecret), mustSecret(s, ctx, ghWebhookSecret), nil, fn); err != nil {
				out.Problem = strings.TrimPrefix(err.Error(), "github: ")
			}
		}
	case active != nil: // command-line configuration
		out.Source, out.Configured, out.Enabled = "flags", true, true
		out.APIBase, out.PublicURL, out.PipelineFile = active.APIBase, active.PublicURL, active.PipelineFile
		out.TokenSet, out.WebhookSecretSet = true, true
		for _, r := range active.Allow {
			out.Rules = append(out.Rules, githubRule{Repo: r.Repo, Branches: r.Branches})
		}
	}
	base := out.PublicURL
	if base == "" {
		base = "https://<your-public-address>"
	}
	out.WebhookURL = strings.TrimRight(base, "/") + "/webhooks/github"
	return out
}

// secretUnreadable reports whether a secret is stored but cannot be opened with the current key.
func (s *Server) secretUnreadable(ctx context.Context, name string) bool {
	var d secretDoc
	if err := s.docs.Get(ctx, kindSecret, name, &d); err != nil {
		return false
	}
	_, err := s.vault.Open(name, d.Sealed)
	return err != nil
}

func mustSecret(s *Server, ctx context.Context, name string) string {
	v, _ := s.internalSecretValue(ctx, name)
	return v
}

func (s *Server) handleGetGitHub(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.githubView(r.Context(), r))
}

type putGitHubRequest struct {
	Enabled      bool         `json:"enabled"`
	APIBase      string       `json:"api_base"`
	PublicURL    string       `json:"public_url"`
	PipelineFile string       `json:"pipeline_file"`
	Rules        []githubRule `json:"rules"`
	// Token replaces the stored token when set; empty keeps the stored one.
	Token string `json:"token"`
	// WebhookSecret replaces the stored secret when set; GenerateWebhookSecret makes a new
	// random one and returns it once.
	WebhookSecret         string `json:"webhook_secret"`
	GenerateWebhookSecret bool   `json:"generate_webhook_secret"`
}

func (s *Server) handlePutGitHub(w http.ResponseWriter, r *http.Request) {
	var req putGitHubRequest
	if !readJSON(w, r, &req) {
		return
	}
	s.ghMu.Lock()
	defer s.ghMu.Unlock()
	ctx := r.Context()
	p := principalFrom(ctx)

	for i := range req.Rules {
		req.Rules[i].Repo = strings.TrimSpace(req.Rules[i].Repo)
		for j := range req.Rules[i].Branches {
			req.Rules[i].Branches[j] = strings.TrimSpace(req.Rules[i].Branches[j])
		}
	}
	st := githubSettings{Enabled: req.Enabled, APIBase: strings.TrimSpace(req.APIBase), PublicURL: strings.TrimSpace(req.PublicURL),
		PipelineFile: strings.TrimSpace(req.PipelineFile), Rules: req.Rules, UpdatedAt: time.Now().UTC(), UpdatedBy: p.Name}
	if st.Rules == nil {
		st.Rules = []githubRule{}
	}
	var prev githubSettings
	_ = s.docs.Get(ctx, kindSettings, idGitHub, &prev)
	st.AppID, st.AppSlug, st.AppURL, st.InstallationID = prev.AppID, prev.AppSlug, prev.AppURL, prev.InstallationID
	var tokenFn func(context.Context) (string, error)
	if st.AppID != 0 && st.InstallationID != 0 && req.Token == "" {
		tokenFn = s.installationTokenFunc(st)
	}

	token, _ := s.internalSecretValue(ctx, ghTokenSecret)
	webhook, _ := s.internalSecretValue(ctx, ghWebhookSecret)
	generated := ""
	switch {
	case req.GenerateWebhookSecret:
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			s.internalError(w, "generating a secret", err)
			return
		}
		generated = hex.EncodeToString(b)
		webhook = generated
	case req.WebhookSecret != "":
		webhook = req.WebhookSecret
	}
	if req.Token != "" {
		token = strings.TrimSpace(req.Token)
	}
	if strings.ContainsAny(token, "\r\n\x00") || strings.ContainsAny(webhook, "\r\n\x00") {
		writeError(w, http.StatusBadRequest, "secrets must not contain newlines")
		return
	}

	// Validate everything before changing anything. A disabled integration is saved as the
	// operator left it, but its repositories must still be well formed.
	if req.Enabled {
		if _, err := buildGitHub(st, token, webhook, s.ghCfg.Load(), tokenFn); err != nil {
			writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "github: "))
			return
		}
	} else if err := checkRules(st.Rules); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Token != "" {
		if _, _, err := s.putSecret(ctx, p.Name, ghTokenSecret, token, "GitHub token"); err != nil {
			s.internalError(w, "storing the token", err)
			return
		}
	}
	if req.GenerateWebhookSecret || req.WebhookSecret != "" {
		if _, _, err := s.putSecret(ctx, p.Name, ghWebhookSecret, webhook, "GitHub webhook secret"); err != nil {
			s.internalError(w, "storing the webhook secret", err)
			return
		}
	}
	if err := s.docs.Put(ctx, kindSettings, idGitHub, st); err != nil {
		s.internalError(w, "saving the settings", err)
		return
	}
	if err := s.reloadGitHub(ctx); err != nil {
		s.log.Error("reloading GitHub settings", "err", err)
	}
	detail := fmt.Sprintf("enabled=%v, %d repositories", st.Enabled, len(st.Rules))
	if req.Token != "" {
		detail += ", token replaced"
	}
	if req.GenerateWebhookSecret {
		detail += ", webhook secret generated"
	} else if req.WebhookSecret != "" {
		detail += ", webhook secret replaced"
	}
	s.auditReq(r, "integration.github.update", "integration/github", detail, true)
	view := s.githubView(ctx, r)
	view.GeneratedWebhookSecret = generated
	writeJSON(w, http.StatusOK, view)
}

func checkRules(rules []githubRule) error {
	for i, rule := range rules {
		if !repoNameRE.MatchString(rule.Repo) {
			return fmt.Errorf("repository %q must look like owner/name", rule.Repo)
		}
		if len(rule.Branches) == 0 {
			return fmt.Errorf("%s: list at least one branch", rule.Repo)
		}
		for _, b := range rule.Branches {
			if _, err := path.Match(b, ""); err != nil || b == "" {
				return fmt.Errorf("rule %d (%s): bad branch pattern %q", i+1, rule.Repo, b)
			}
		}
	}
	return nil
}

func (s *Server) handleDeleteGitHub(w http.ResponseWriter, r *http.Request) {
	s.ghMu.Lock()
	defer s.ghMu.Unlock()
	ctx := r.Context()
	for _, name := range []string{ghTokenSecret, ghWebhookSecret, ghAppKeySecret} {
		if err := s.docs.Delete(ctx, kindSecret, name); err != nil && !errors.Is(err, docstore.ErrNotFound) {
			s.internalError(w, "removing a secret", err)
			return
		}
	}
	if err := s.docs.Delete(ctx, kindSettings, idGitHub); err != nil && !errors.Is(err, docstore.ErrNotFound) {
		s.internalError(w, "removing the settings", err)
		return
	}
	s.ghCfg.Store(nil)
	s.ghSource.Store("none")
	s.auditReq(r, "integration.github.delete", "integration/github", "", true)
	w.WriteHeader(http.StatusNoContent)
}

// RepoCheck is the result of testing access to one repository.
type RepoCheck struct {
	Repo    string `json:"repo"`
	OK      bool   `json:"ok"`
	Status  int    `json:"status,omitempty"`
	Message string `json:"message"`
}

// handleTestGitHub checks, with the stored token, that every allowed repository can be read.
func (s *Server) handleTestGitHub(w http.ResponseWriter, r *http.Request) {
	gh := s.gh()
	if gh == nil {
		writeError(w, http.StatusConflict, "the integration is not active; save it with Enabled on first")
		return
	}
	results := []RepoCheck{}
	for _, rule := range gh.Allow {
		results = append(results, s.checkRepo(r.Context(), gh, rule.Repo))
	}
	ok := true
	for _, c := range results {
		ok = ok && c.OK
	}
	s.auditReq(r, "integration.github.test", "integration/github", fmt.Sprintf("%d repositories, all ok: %v", len(results), ok), ok)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "results": results})
}

func (s *Server) checkRepo(ctx context.Context, gh *GitHubConfig, repo string) RepoCheck {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gh.APIBase+"/repos/"+repo, nil)
	if err != nil {
		return RepoCheck{Repo: repo, Message: err.Error()}
	}
	if err := s.githubHeaders(req); err != nil {
		return RepoCheck{Repo: repo, Message: err.Error()}
	}
	resp, err := gh.HTTPClient.Do(req)
	if err != nil {
		return RepoCheck{Repo: repo, Message: "could not reach GitHub: " + err.Error()}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	c := RepoCheck{Repo: repo, Status: resp.StatusCode}
	switch resp.StatusCode {
	case http.StatusOK:
		c.OK, c.Message = true, "readable with this token"
	case http.StatusUnauthorized:
		c.Message = "GitHub rejected the token (expired or wrong)"
	case http.StatusForbidden:
		c.Message = "the token is not allowed to read this repository (check its permissions or SSO authorisation)"
	case http.StatusNotFound:
		c.Message = "not found: the repository name is wrong, or the token cannot see it"
	default:
		c.Message = fmt.Sprintf("GitHub answered %d", resp.StatusCode)
	}
	return c
}

// GHAppJSON is the GitHub App connected from the console.
type GHAppJSON struct {
	ID             int64  `json:"id"`
	Slug           string `json:"slug"`
	URL            string `json:"url,omitempty"`
	InstallationID int64  `json:"installation_id,omitempty"`
}

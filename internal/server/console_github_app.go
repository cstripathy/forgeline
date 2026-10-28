package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/forgelinehq/forgeline/internal/docstore"
)

// Connecting GitHub from the console with a GitHub App. The administrator clicks one button,
// GitHub shows one approval page, and Forgeline receives the app's identity, private key and
// webhook secret straight from GitHub (the "manifest flow"), so nothing is copied by hand. The
// app asks only for read access to contents and write access to commit statuses, is private to its
// owner, and signs in with short-lived installation tokens. See design note 010.

const (
	ghAppKeySecret = "forgeline.github.app_key"
	ghAppStateTTL  = 15 * time.Minute
	ghAppMaxStates = 8
)

var ghOwnerRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

type ghAppState struct {
	by      string
	expires time.Time
	apiBase string
}

type ghAppRegistry struct {
	mu     sync.Mutex
	states map[string]ghAppState
}

func (g *ghAppRegistry) add(by, apiBase string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.states == nil {
		g.states = map[string]ghAppState{}
	}
	now := time.Now()
	for k, v := range g.states {
		if now.After(v.expires) {
			delete(g.states, k)
		}
	}
	if len(g.states) >= ghAppMaxStates {
		return "", false
	}
	id := ghRandomID()
	g.states[id] = ghAppState{by: by, expires: now.Add(ghAppStateTTL), apiBase: apiBase}
	return id, true
}

// peek returns a state without using it; take uses it up.
func (g *ghAppRegistry) peek(id string) (ghAppState, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.states[id]
	if !ok || time.Now().After(st.expires) {
		delete(g.states, id)
		return ghAppState{}, false
	}
	return st, true
}

func (g *ghAppRegistry) take(id string) (ghAppState, bool) {
	st, ok := g.peek(id)
	if ok {
		g.mu.Lock()
		delete(g.states, id)
		g.mu.Unlock()
	}
	return st, ok
}

// --- GitHub's addresses ---

// githubWebBase is where the person's browser goes: github.com for the public API, the host
// itself for GitHub Enterprise Server (whose API lives under /api/v3), and the API's own address
// for a loopback stand-in.
func githubWebBase(apiBase string) string {
	if apiBase == "" || apiBase == "https://api.github.com" {
		return "https://github.com"
	}
	return strings.TrimSuffix(strings.TrimRight(apiBase, "/"), "/api/v3")
}

func (s *Server) ghAPIBase(st githubSettings) string {
	if st.APIBase != "" {
		return strings.TrimRight(st.APIBase, "/")
	}
	return "https://api.github.com"
}

func (s *Server) ghClient() *http.Client {
	if gh := s.gh(); gh != nil && gh.HTTPClient != nil {
		return gh.HTTPClient
	}
	return &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// --- the app's own credentials ---

// appJWT signs the short token that proves we are the app (RS256, nine minutes).
func appJWT(pemKey string, appID int64, now time.Time) (string, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return "", errors.New("the stored GitHub App key is not a PEM key")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else if k8, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k8.(*rsa.PrivateKey); ok {
			key = rk
		}
	}
	if key == nil {
		return "", errors.New("the stored GitHub App key could not be read")
	}
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(appID, 10)})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// ghAPI makes one call to the GitHub API, as the app (JWT) or with a given bearer token.
func (s *Server) ghAPI(ctx context.Context, st githubSettings, method, path, bearer string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.ghAPIBase(st)+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "forgeline")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := s.ghClient().Do(req)
	if err != nil {
		return 0, errors.New("could not reach GitHub")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		var e struct{ Message string }
		json.Unmarshal(raw, &e)
		return resp.StatusCode, fmt.Errorf("GitHub answered %d: %s", resp.StatusCode, truncate(firstLine(e.Message), 160))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, errors.New("GitHub sent a reply that could not be read")
		}
	}
	return resp.StatusCode, nil
}

// appAuth returns a signed app token for the stored app.
func (s *Server) appAuth(ctx context.Context, st githubSettings) (string, error) {
	if st.AppID == 0 {
		return "", errors.New("no GitHub App is set up")
	}
	key, ok := s.internalSecretValue(ctx, ghAppKeySecret)
	if !ok {
		return "", errors.New("the GitHub App key is missing or cannot be opened with this server's master key")
	}
	return appJWT(key, st.AppID, time.Now())
}

// ghTokenCache holds one installation's current token, shared by everything that needs it.
type ghTokenCache struct {
	mu    sync.Mutex
	token string
	exp   time.Time
}

// installationTokenFunc returns a function that yields a current installation token, asking
// GitHub for a new one when the cached one is within five minutes of expiring. The cache is
// shared per installation, so a burst of requests mints one token.
func (s *Server) installationTokenFunc(st githubSettings) func(context.Context) (string, error) {
	c, _ := s.ghTokens.LoadOrStore(fmt.Sprintf("%d/%d", st.AppID, st.InstallationID), &ghTokenCache{})
	cache := c.(*ghTokenCache)
	return func(ctx context.Context) (string, error) {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if cache.token != "" && time.Until(cache.exp) > 5*time.Minute {
			return cache.token, nil
		}
		jwt, err := s.appAuth(ctx, st)
		if err != nil {
			return "", err
		}
		var out struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if _, err := s.ghAPI(ctx, st, http.MethodPost, "/app/installations/"+strconv.FormatInt(st.InstallationID, 10)+"/access_tokens", jwt, map[string]any{}, &out); err != nil {
			return "", fmt.Errorf("could not get an installation token: %w", err)
		}
		if out.Token == "" {
			return "", errors.New("GitHub sent no installation token")
		}
		cache.token, cache.exp = out.Token, out.ExpiresAt
		if cache.exp.IsZero() {
			cache.exp = time.Now().Add(time.Hour)
		}
		return cache.token, nil
	}
}

// --- step 1: start ---

type ghAppStartRequest struct {
	PublicURL string `json:"public_url"`
	Name      string `json:"name"`
	Owner     string `json:"owner"` // an organisation to own the app; empty means the signed-in person
	// APIBase is for GitHub Enterprise Server (https://host/api/v3) and for tests (loopback http).
	APIBase string `json:"api_base"`
}

// GHAppStartJSON tells the console where to send the person.
type GHAppStartJSON struct {
	LaunchURL  string `json:"launch_url"`
	HookActive bool   `json:"hook_active"`
	HookNote   string `json:"hook_note,omitempty"`
}

func (s *Server) handleGHAppStart(w http.ResponseWriter, r *http.Request) {
	var req ghAppStartRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.PublicURL, req.Name, req.Owner = strings.TrimRight(strings.TrimSpace(req.PublicURL), "/"), strings.TrimSpace(req.Name), strings.TrimSpace(req.Owner)
	if req.Owner != "" && !ghOwnerRE.MatchString(req.Owner) {
		writeError(w, http.StatusBadRequest, "the organisation name is not valid")
		return
	}
	if len(req.Name) > 34 || strings.ContainsAny(req.Name, "<>\"'&\r\n") {
		writeError(w, http.StatusBadRequest, "the app name must be at most 34 characters, without quotes or angle brackets")
		return
	}
	if req.PublicURL != "" {
		if u, err := url.Parse(req.PublicURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Path != "" {
			writeError(w, http.StatusBadRequest, "the public address must look like https://ci.example.com")
			return
		}
	}
	origin := r.Header.Get("Origin")
	if origin == "" && req.PublicURL == "" {
		writeError(w, http.StatusBadRequest, "enter this server's public address")
		return
	}
	var prev githubSettings
	_ = s.docs.Get(r.Context(), kindSettings, idGitHub, &prev)
	if prev.AppID != 0 {
		writeError(w, http.StatusConflict, "a GitHub App is already connected. Remove the GitHub integration first to create another")
		return
	}
	apiBase := s.ghAPIBase(prev)
	if ab := strings.TrimRight(strings.TrimSpace(req.APIBase), "/"); ab != "" {
		u, err := url.Parse(ab)
		loop := err == nil && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || (u.Scheme != "https" && !(u.Scheme == "http" && loop)) || (u.Path != "" && u.Path != "/api/v3") {
			writeError(w, http.StatusBadRequest, "the GitHub API address must be https (plain http only for localhost), such as https://github.example.com/api/v3")
			return
		}
		apiBase = ab
	}
	p := principalFrom(r.Context())
	id, ok := s.ghApp.add(p.Name, apiBase)
	if !ok {
		writeError(w, http.StatusTooManyRequests, "too many GitHub App setups are waiting. Finish or wait for one to expire")
		return
	}
	s.ghApp.mu.Lock()
	st := s.ghApp.states[id]
	st.apiBase = apiBase
	s.ghApp.states[id] = st
	s.ghApp.mu.Unlock()
	// Remember what the launch page needs. The state is the only thing in the URL.
	s.ghAppForm.Store(id, ghAppForm{origin: origin, publicURL: req.PublicURL, name: req.Name, owner: req.Owner})

	hookBase := req.PublicURL
	if hookBase == "" {
		hookBase = origin
	}
	active, note := hookReachable(hookBase)
	s.auditReq(r, "integration.github.app.start", "integration/github", "GitHub App setup started", true)
	writeJSON(w, http.StatusOK, GHAppStartJSON{LaunchURL: "/v1/integrations/github/app/launch?state=" + id, HookActive: active, HookNote: note})
}

type ghAppForm struct {
	origin, publicURL, name, owner string
}

// hookReachable says whether GitHub could deliver webhooks to base. A laptop address cannot be.
func hookReachable(base string) (bool, string) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return false, "The webhook address is not valid."
	}
	h := u.Hostname()
	if h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasSuffix(h, ".local") || u.Scheme != "https" {
		return false, "GitHub cannot reach this address, so webhooks are created switched off. Give this server a public https address (or a tunnel) and edit the app on GitHub, and pushes will then start runs."
	}
	return true, ""
}

// --- step 2: the page that hands the manifest to GitHub ---

// manifestPage is shown to the administrator. It exists because the manifest must be sent to
// GitHub as a form post, and the console's own pages forbid forms. It has no script, and its
// policy allows a form to go only to the one GitHub address.
var manifestPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Connect GitHub · Forgeline</title><link rel="icon" href="/ui/favicon.svg"><link rel="stylesheet" href="/ui/app.css"></head>
<body class="min-h-screen bg-slate-50 text-slate-900 dark:bg-slate-950 dark:text-slate-100"><main class="mx-auto mt-24 max-w-md p-4"><div class="card card-pad">
<h1 class="text-lg font-semibold">Create the Forgeline GitHub App</h1>
<p class="mt-2 text-sm">GitHub will show you the app: <strong>%[1]s</strong>, private to %[2]s, with read access to repository contents and write access to commit statuses. Review it there and choose Create. You come back here afterwards.</p>
<form method="post" action="%[3]s" class="mt-5"><input type="hidden" name="manifest" value="%[4]s"><button class="btn-primary" type="submit" id="go-github">Continue to GitHub</button>
<a class="btn-secondary ml-2" href="/#/integrations">Cancel</a></form></div></main></body></html>`

func (s *Server) handleGHAppLaunch(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("state")
	st, ok := s.ghApp.peek(id)
	p := principalFrom(r.Context())
	if !ok || st.by != p.Name {
		http.Error(w, "this setup link expired. Start again from Integrations.", http.StatusGone)
		return
	}
	f, ok := s.ghAppForm.Load(id)
	if !ok {
		http.Error(w, "this setup link expired. Start again from Integrations.", http.StatusGone)
		return
	}
	form := f.(ghAppForm)
	callbackBase := form.origin
	if callbackBase == "" {
		callbackBase = form.publicURL
	}
	hookBase := form.publicURL
	if hookBase == "" {
		hookBase = form.origin
	}
	active, _ := hookReachable(hookBase)
	name := form.name
	if name == "" {
		name = "Forgeline " + ghRandomID()[:6]
	}
	manifest := map[string]any{
		"name":                name,
		"url":                 hookBase,
		"hook_attributes":     map[string]any{"url": hookBase + "/webhooks/github", "active": active},
		"redirect_url":        callbackBase + "/v1/integrations/github/app/callback",
		"public":              false,
		"default_permissions": map[string]string{"contents": "read", "statuses": "write", "metadata": "read", "pull_requests": "read"},
		"default_events":      []string{"push", "pull_request"},
	}
	mb, _ := json.Marshal(manifest)
	web := githubWebBase(st.apiBase)
	post := web + "/settings/apps/new?state=" + url.QueryEscape(id)
	owner := "your GitHub account"
	if form.owner != "" {
		post = web + "/organizations/" + url.PathEscape(form.owner) + "/settings/apps/new?state=" + url.QueryEscape(id)
		owner = "the " + form.owner + " organisation"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self' data:; form-action "+web+"; base-uri 'none'; frame-ancestors 'none'")
	fmt.Fprintf(w, manifestPage, html.EscapeString(name), html.EscapeString(owner), html.EscapeString(post), html.EscapeString(string(mb)))
}

// --- step 3: GitHub sends the person back ---

// handleGHAppCallback receives the one-time code. It carries no cookie (the visitor arrives from
// another site), so it is bound to the setup the administrator started by the state value.
func (s *Server) handleGHAppCallback(w http.ResponseWriter, r *http.Request) {
	code, state := r.URL.Query().Get("code"), r.URL.Query().Get("state")
	back := func(msg string) {
		http.Redirect(w, r, "/#/integrations?app="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	st, ok := s.ghApp.take(state)
	if !ok {
		back("expired")
		return
	}
	var publicURL string
	if f, ok := s.ghAppForm.LoadAndDelete(state); ok {
		publicURL = f.(ghAppForm).publicURL
	}
	if code == "" || len(code) > 200 || strings.ContainsAny(code, "/?#% ") {
		back("failed")
		return
	}
	ctx := r.Context()
	actor := Principal{Name: st.by}
	ip := clientIP(r)
	var conv struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		HTMLURL       string `json:"html_url"`
		WebhookSecret string `json:"webhook_secret"`
		PEM           string `json:"pem"`
	}
	if _, err := s.ghAPI(ctx, githubSettings{APIBase: st.apiBase}, http.MethodPost, "/app-manifests/"+code+"/conversions", "", nil, &conv); err != nil || conv.ID == 0 || conv.PEM == "" {
		s.audit(ctx, actor, ip, "integration.github.app.create", "integration/github", "GitHub did not return the app: "+ghErrText(err), false)
		back("failed")
		return
	}
	if _, _, err := s.putSecret(ctx, st.by, ghAppKeySecret, conv.PEM, "GitHub App private key"); err != nil {
		s.audit(ctx, actor, ip, "integration.github.app.create", "integration/github", "could not store the key", false)
		back("failed")
		return
	}
	if conv.WebhookSecret != "" {
		if _, _, err := s.putSecret(ctx, st.by, ghWebhookSecret, conv.WebhookSecret, "GitHub webhook secret"); err != nil {
			back("failed")
			return
		}
	}
	s.ghMu.Lock()
	settings := githubSettings{Enabled: false, APIBase: st.apiBase, Rules: []githubRule{}, AppID: conv.ID, AppSlug: conv.Slug, AppURL: conv.HTMLURL, PublicURL: publicURL, UpdatedAt: time.Now().UTC(), UpdatedBy: st.by}
	if st.apiBase == "https://api.github.com" {
		settings.APIBase = ""
	}
	err := s.docs.Put(ctx, kindSettings, idGitHub, settings)
	s.ghMu.Unlock()
	if err != nil {
		back("failed")
		return
	}
	s.audit(ctx, actor, ip, "integration.github.app.create", "integration/github", "GitHub App "+conv.Slug+" created", true)
	back("created")
}

// --- step 4: choose the installation ---

// GHInstallationJSON is one place the app is installed.
type GHInstallationJSON struct {
	ID        int64  `json:"id"`
	Account   string `json:"account"`
	Selection string `json:"repository_selection"`
}

func (s *Server) handleGHAppInstallations(w http.ResponseWriter, r *http.Request) {
	var st githubSettings
	if err := s.docs.Get(r.Context(), kindSettings, idGitHub, &st); err != nil || st.AppID == 0 {
		writeError(w, http.StatusConflict, "create the GitHub App first")
		return
	}
	jwt, err := s.appAuth(r.Context(), st)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	var list []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
		Selection string `json:"repository_selection"`
	}
	if _, err := s.ghAPI(r.Context(), st, http.MethodGet, "/app/installations?per_page=50", jwt, nil, &list); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := []GHInstallationJSON{}
	for _, i := range list {
		out = append(out, GHInstallationJSON{ID: i.ID, Account: i.Account.Login, Selection: i.Selection})
	}
	writeJSON(w, http.StatusOK, map[string]any{"installations": out, "install_url": githubWebBase(s.ghAPIBase(st)) + "/apps/" + st.AppSlug + "/installations/new", "settings_url": st.AppURL})
}

// GHRepoJSON is a repository the installation can reach.
type GHRepoJSON struct {
	FullName string `json:"full_name"`
	Default  string `json:"default_branch"`
	Private  bool   `json:"private"`
}

func (s *Server) installationRepos(ctx context.Context, st githubSettings) ([]GHRepoJSON, error) {
	tok, err := s.installationTokenFunc(st)(ctx)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Repositories []struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
			Private       bool   `json:"private"`
		} `json:"repositories"`
	}
	if _, err := s.ghAPI(ctx, st, http.MethodGet, "/installation/repositories?per_page=100", tok, nil, &resp); err != nil {
		return nil, err
	}
	out := []GHRepoJSON{}
	for _, r := range resp.Repositories {
		out = append(out, GHRepoJSON{FullName: r.FullName, Default: r.DefaultBranch, Private: r.Private})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName < out[j].FullName })
	return out, nil
}

type ghAppInstallRequest struct {
	InstallationID int64        `json:"installation_id"`
	Rules          []githubRule `json:"rules"`
	PublicURL      string       `json:"public_url"`
}

// handleGHAppInstall uses an installation: proves it belongs to our app, lists its repositories
// and turns the integration on for the chosen ones.
func (s *Server) handleGHAppInstall(w http.ResponseWriter, r *http.Request) {
	var req ghAppInstallRequest
	if !readJSON(w, r, &req) {
		return
	}
	s.ghMu.Lock()
	defer s.ghMu.Unlock()
	ctx := r.Context()
	var st githubSettings
	if err := s.docs.Get(ctx, kindSettings, idGitHub, &st); err != nil || st.AppID == 0 {
		writeError(w, http.StatusConflict, "create the GitHub App first")
		return
	}
	if req.InstallationID <= 0 {
		writeError(w, http.StatusBadRequest, "choose where the app is installed")
		return
	}
	jwt, err := s.appAuth(ctx, st)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	// GitHub answers only for installations of this app, which is what makes the id trustworthy.
	if _, err := s.ghAPI(ctx, st, http.MethodGet, "/app/installations/"+strconv.FormatInt(req.InstallationID, 10), jwt, nil, nil); err != nil {
		writeError(w, http.StatusBadRequest, "that is not an installation of this app: "+err.Error())
		return
	}
	for i := range req.Rules {
		req.Rules[i].Repo = strings.TrimSpace(req.Rules[i].Repo)
		for j := range req.Rules[i].Branches {
			req.Rules[i].Branches[j] = strings.TrimSpace(req.Rules[i].Branches[j])
		}
	}
	next := st
	next.InstallationID = req.InstallationID
	next.Rules = req.Rules
	if next.Rules == nil {
		next.Rules = []githubRule{}
	}
	if pu := strings.TrimRight(strings.TrimSpace(req.PublicURL), "/"); pu != "" {
		next.PublicURL = pu
	}
	next.Enabled = len(next.Rules) > 0
	p := principalFrom(ctx)
	next.UpdatedAt, next.UpdatedBy = time.Now().UTC(), p.Name
	if next.Enabled {
		webhook, _ := s.internalSecretValue(ctx, ghWebhookSecret)
		if _, err := buildGitHub(next, "", webhook, s.ghCfg.Load(), s.installationTokenFunc(next)); err != nil {
			writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "github: "))
			return
		}
		// Every chosen repository must be reachable by the installation.
		repos, err := s.installationRepos(ctx, next)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		have := map[string]bool{}
		for _, rp := range repos {
			have[strings.ToLower(rp.FullName)] = true
		}
		for _, rule := range next.Rules {
			if !have[strings.ToLower(rule.Repo)] {
				writeError(w, http.StatusBadRequest, rule.Repo+" is not part of this installation. Give the app access to it on GitHub first")
				return
			}
		}
	}
	if err := s.docs.Put(ctx, kindSettings, idGitHub, next); err != nil {
		s.internalError(w, "saving the settings", err)
		return
	}
	if err := s.reloadGitHub(ctx); err != nil {
		s.log.Error("reloading GitHub settings", "err", err)
	}
	s.auditReq(r, "integration.github.app.install", "integration/github", fmt.Sprintf("installation %d, %d repositories", next.InstallationID, len(next.Rules)), true)
	writeJSON(w, http.StatusOK, s.githubView(ctx, r))
}

func (s *Server) handleGHAppRepos(w http.ResponseWriter, r *http.Request) {
	var st githubSettings
	if err := s.docs.Get(r.Context(), kindSettings, idGitHub, &st); err != nil || st.AppID == 0 {
		writeError(w, http.StatusConflict, "create the GitHub App first")
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if id <= 0 {
		id = st.InstallationID
	}
	if id <= 0 {
		writeError(w, http.StatusBadRequest, "choose an installation")
		return
	}
	jwt, err := s.appAuth(r.Context(), st)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	// Check the id belongs to this app before minting a token for it.
	if _, err := s.ghAPI(r.Context(), st, http.MethodGet, "/app/installations/"+strconv.FormatInt(id, 10), jwt, nil, nil); err != nil {
		writeError(w, http.StatusBadRequest, "that is not an installation of this app")
		return
	}
	probe := st
	probe.InstallationID = id
	repos, err := s.installationRepos(r.Context(), probe)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": repos})
}

var _ = docstore.ErrNotFound

func ghRandomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func ghErrText(err error) string {
	if err == nil {
		return "no details"
	}
	return err.Error()
}

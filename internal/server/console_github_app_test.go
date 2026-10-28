package server

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cstripathy/forgeline/internal/authn"
	"github.com/cstripathy/forgeline/internal/executor"
	"github.com/cstripathy/forgeline/internal/store"
)

const (
	appID         = 4242
	appInstall    = 77
	appWebhookSec = "whsec-0123456789abcdef-from-github"
	appGoodCode   = "good-manifest-code"
)

// fakeGHApp is GitHub as far as the manifest flow and app tokens go, in front of fakeGitHub for
// the repository calls. It checks the app's signed token with the real public key.
type fakeGHApp struct {
	gh  *fakeGitHub
	srv *httptest.Server
	key *rsa.PrivateKey
	pem string

	mu       sync.Mutex
	minted   int
	ttl      time.Duration
	jwtBad   int
	lastAuth string
	tokens   map[string]bool
	converts int
}

func newFakeGHApp(t *testing.T, gh *fakeGitHub) *fakeGHApp {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGHApp{gh: gh, key: key, ttl: time.Hour, tokens: map[string]bool{},
		pem: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// jwtOK verifies an app token the way GitHub does.
func (f *fakeGHApp) jwtOK(auth string) bool {
	parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss      string
		Iat, Exp int64
	}
	json.Unmarshal(raw, &c)
	now := time.Now().Unix()
	return c.Iss == fmt.Sprint(appID) && c.Iat <= now && c.Exp > now && c.Exp-c.Iat <= 11*60
}

func (f *fakeGHApp) serve(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, v any) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	auth := r.Header.Get("Authorization")
	needJWT := func() bool {
		if !f.jwtOK(auth) {
			f.mu.Lock()
			f.jwtBad++
			f.mu.Unlock()
			reply(401, map[string]string{"message": "A JSON web token could not be decoded"})
			return false
		}
		return true
	}
	switch {
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/app-manifests/") && strings.HasSuffix(r.URL.Path, "/conversions"):
		f.mu.Lock()
		f.converts++
		f.mu.Unlock()
		if r.URL.Path != "/app-manifests/"+appGoodCode+"/conversions" {
			reply(404, map[string]string{"message": "Not Found"})
			return
		}
		reply(201, map[string]any{"id": appID, "slug": "forgeline-test", "html_url": "https://github.com/apps/forgeline-test",
			"webhook_secret": appWebhookSec, "pem": f.pem, "client_secret": "must-not-be-kept"})
	case r.Method == "GET" && r.URL.Path == "/app/installations":
		if needJWT() {
			reply(200, []map[string]any{{"id": appInstall, "account": map[string]string{"login": "acme"}, "repository_selection": "selected"}})
		}
	case r.Method == "GET" && r.URL.Path == fmt.Sprintf("/app/installations/%d", appInstall):
		if needJWT() {
			reply(200, map[string]any{"id": appInstall})
		}
	case r.Method == "POST" && r.URL.Path == fmt.Sprintf("/app/installations/%d/access_tokens", appInstall):
		if !needJWT() {
			return
		}
		f.mu.Lock()
		f.minted++
		tok := fmt.Sprintf("ghs_installation_token_%d", f.minted)
		f.tokens[tok] = true
		exp := time.Now().Add(f.ttl)
		f.mu.Unlock()
		f.gh.allow(tok)
		reply(201, map[string]any{"token": tok, "expires_at": exp.UTC().Format(time.RFC3339)})
	case strings.HasPrefix(r.URL.Path, "/app/installations/"):
		if needJWT() {
			reply(404, map[string]string{"message": "Not Found"})
		}
	case r.Method == "GET" && r.URL.Path == "/installation/repositories":
		if !f.gh.tokenOK(auth) {
			reply(401, map[string]string{"message": "Bad credentials"})
			return
		}
		reply(200, map[string]any{"repositories": []map[string]any{{"full_name": ghRepo, "default_branch": "main", "private": true}}})
	default:
		f.gh.serve(w, r)
	}
}

func (f *fakeGHApp) mintedCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.minted }

// startApp begins the manifest flow as the console does and returns the state id.
func startApp(t *testing.T, admin *browser, api string, extra map[string]any) (state string, start GHAppStartJSON) {
	t.Helper()
	req := map[string]any{"public_url": "https://ci.example.com", "api_base": api}
	for k, v := range extra {
		req[k] = v
	}
	json.Unmarshal(admin.expect(200, "POST", "/v1/integrations/github/app/start", req), &start)
	u, _ := url.Parse(start.LaunchURL)
	return u.Query().Get("state"), start
}

func TestConsoleConnectsGitHubWithAnApp(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addCommit(ghRepo, sha1st, map[string]string{"forgeline.yaml": ghPipeline})
	app := newFakeGHApp(t, gh)
	h := newHarness(t, &executor.Fake{})
	admin := h.adminBrowser("ada")
	admin.createUser("grace", authn.Admin)
	admin.createUser("dev", authn.Developer)
	dev, grace := h.login("dev"), h.login("grace")

	// Only administrators start; the inputs are checked.
	dev.expect(403, "POST", "/v1/integrations/github/app/start", map[string]any{"public_url": "https://ci.example.com", "api_base": app.srv.URL})
	admin.expect(400, "POST", "/v1/integrations/github/app/start", map[string]any{"public_url": "ci.example.com", "api_base": app.srv.URL})
	admin.expect(400, "POST", "/v1/integrations/github/app/start", map[string]any{"public_url": "https://ci.example.com", "owner": "bad owner!", "api_base": app.srv.URL})
	admin.expect(400, "POST", "/v1/integrations/github/app/start", map[string]any{"public_url": "https://ci.example.com", "name": "<script>", "api_base": app.srv.URL})
	admin.expect(400, "POST", "/v1/integrations/github/app/start", map[string]any{"public_url": "https://ci.example.com", "api_base": "http://evil.example.com"})
	admin.expect(409, "GET", "/v1/integrations/github/app/installations", nil) // no app yet

	state, start := startApp(t, admin, app.srv.URL, map[string]any{"name": "Forgeline Test"})
	if !start.HookActive || state == "" {
		t.Fatalf("start = %+v", start)
	}

	// The launch page belongs to whoever started it, carries a policy that lets a form go only to
	// GitHub, has no script, and holds a manifest that asks for the least it needs.
	if resp, _ := grace.do("GET", start.LaunchURL, nil); resp.StatusCode != http.StatusGone {
		t.Errorf("another administrator's launch = %d, want 410", resp.StatusCode)
	}
	resp, page := admin.do("GET", start.LaunchURL, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("launch = %d %s", resp.StatusCode, page)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action "+app.srv.URL) || strings.Contains(csp, "script-src") || strings.Contains(csp, "unsafe") {
		t.Errorf("launch policy = %q", csp)
	}
	body := string(page)
	if strings.Contains(body, "<script") || !strings.Contains(body, `action="`+app.srv.URL+`/settings/apps/new?state=`+state+`"`) {
		t.Errorf("launch page:\n%s", body)
	}
	raw := body[strings.Index(body, `name="manifest" value="`)+len(`name="manifest" value="`):]
	raw = raw[:strings.Index(raw, `"`)]
	var manifest struct {
		Name     string `json:"name"`
		Public   bool   `json:"public"`
		Redirect string `json:"redirect_url"`
		Hook     struct {
			URL    string
			Active bool
		} `json:"hook_attributes"`
		Perms  map[string]string `json:"default_permissions"`
		Events []string          `json:"default_events"`
	}
	if err := json.Unmarshal([]byte(strings.NewReplacer("&#34;", `"`, "&amp;", "&", "&lt;", "<", "&gt;", ">", "&#39;", "'").Replace(raw)), &manifest); err != nil {
		t.Fatalf("manifest: %v\n%s", err, raw)
	}
	if manifest.Name != "Forgeline Test" || manifest.Public || manifest.Redirect != "https://ci.example.com/v1/integrations/github/app/callback" ||
		manifest.Hook.URL != "https://ci.example.com/webhooks/github" || !manifest.Hook.Active {
		t.Errorf("manifest = %+v", manifest)
	}
	if manifest.Perms["contents"] != "read" || manifest.Perms["statuses"] != "write" || len(manifest.Perms) != 4 {
		t.Errorf("permissions = %v", manifest.Perms)
	}

	// GitHub sends the person back with no cookie. A wrong state or code stores nothing.
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	callback := func(code, st string) string {
		resp, err := anon.Get(h.http.URL + "/v1/integrations/github/app/callback?code=" + code + "&state=" + st)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	if loc := callback(appGoodCode, "made-up-state"); !strings.Contains(loc, "app=expired") || app.converts != 0 {
		t.Errorf("unknown state: %s (conversions %d)", loc, app.converts)
	}
	if loc := callback("wrong-code", state); !strings.Contains(loc, "app=failed") {
		t.Errorf("wrong code: %s", loc)
	}
	// A failed attempt used the state up; start again.
	state, start = startApp(t, admin, app.srv.URL, nil)
	if loc := callback(appGoodCode, state); loc != "/#/integrations?app=created" {
		t.Fatalf("callback went to %q", loc)
	}
	if loc := callback(appGoodCode, state); !strings.Contains(loc, "app=expired") {
		t.Errorf("a state was usable twice: %s", loc)
	}
	admin.expect(409, "POST", "/v1/integrations/github/app/start", map[string]any{"public_url": "https://ci.example.com", "api_base": app.srv.URL}) // already has an app

	var view GitHubJSON
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/github", nil), &view)
	if view.PublicURL != "https://ci.example.com" {
		t.Errorf("the public address typed at setup was not kept: %q", view.PublicURL)
	}
	if view.App == nil || view.App.Slug != "forgeline-test" || view.App.InstallationID != 0 || view.Enabled || !view.WebhookSecretSet {
		t.Fatalf("after create: %+v / %+v", view, view.App)
	}

	// Nothing from GitHub's answer is readable afterwards: not the key, the webhook secret, or
	// the client secret.
	for _, path := range []string{"/v1/integrations/github", "/v1/secrets", "/v1/audit", "/v1/overview", "/v1/users", "/v1/integrations/github/app/installations"} {
		_, b := admin.do("GET", path, nil)
		for _, leak := range []string{appWebhookSec, "BEGIN RSA PRIVATE KEY", "must-not-be-kept"} {
			if strings.Contains(string(b), leak) {
				t.Errorf("GET %s exposes %q", path, leak)
			}
		}
	}

	// Installations come from GitHub, asked for with the app's signed token.
	var inst struct {
		Installations []GHInstallationJSON
		InstallURL    string `json:"install_url"`
	}
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/github/app/installations", nil), &inst)
	if len(inst.Installations) != 1 || inst.Installations[0].ID != appInstall || inst.Installations[0].Account != "acme" || !strings.HasSuffix(inst.InstallURL, "/apps/forgeline-test/installations/new") {
		t.Fatalf("installations = %+v", inst)
	}
	if app.jwtBad != 0 {
		t.Errorf("GitHub rejected %d of our app tokens", app.jwtBad)
	}

	// An installation that is not this app's is refused, as is a repository it cannot reach.
	admin.expect(400, "POST", "/v1/integrations/github/app/install", map[string]any{"installation_id": 999, "rules": []map[string]any{{"repo": ghRepo, "branches": []string{"main"}}}})
	admin.expect(400, "POST", "/v1/integrations/github/app/install", map[string]any{"installation_id": appInstall, "rules": []map[string]any{{"repo": "acme/other", "branches": []string{"main"}}}})
	dev.expect(403, "POST", "/v1/integrations/github/app/install", map[string]any{"installation_id": appInstall})

	var repos struct{ Repositories []GHRepoJSON }
	json.Unmarshal(admin.expect(200, "GET", fmt.Sprintf("/v1/integrations/github/app/repos?installation_id=%d", appInstall), nil), &repos)
	if len(repos.Repositories) != 1 || repos.Repositories[0].FullName != ghRepo {
		t.Fatalf("repos = %+v", repos)
	}
	json.Unmarshal(admin.expect(200, "POST", "/v1/integrations/github/app/install", map[string]any{"installation_id": appInstall, "public_url": "https://ci.example.com",
		"rules": []map[string]any{{"repo": ghRepo, "branches": []string{"main"}}}}), &view)
	if !view.Active || !view.Enabled || view.App == nil || view.App.InstallationID != appInstall {
		t.Fatalf("after install: %+v", view)
	}

	// The app's own webhook secret now starts runs, and the run is fetched and reported with an
	// installation token, not with any personal token.
	resp2, out := h.signedWebhook(appWebhookSec, "push", "app-1", pushEvent(ghRepo, "refs/heads/main", sha1st))
	if resp2.StatusCode != 202 {
		t.Fatalf("push = %d %s", resp2.StatusCode, out)
	}
	gh.waitStates(h, sha1st, "pending", "success")
	runs := h.runs()
	if len(runs) != 1 || runs[0].Trigger != store.TriggerGitHub {
		t.Fatalf("runs = %+v", runs)
	}
	if app.mintedCount() < 1 || app.jwtBad != 0 {
		t.Errorf("tokens minted %d, bad app tokens %d", app.mintedCount(), app.jwtBad)
	}
	first := app.mintedCount()
	// Saved settings keep the app when the form is saved again without a token.
	admin.expect(200, "PUT", "/v1/integrations/github", map[string]any{"enabled": true, "public_url": "https://ci.example.com",
		"rules": []map[string]any{{"repo": ghRepo, "branches": []string{"main"}}}})
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/github", nil), &view)
	if view.App == nil || view.App.InstallationID != appInstall || !view.Active {
		t.Errorf("saving the form lost the app: %+v", view)
	}
	resp2, _ = h.signedWebhook(appWebhookSec, "push", "app-2", pushEvent(ghRepo, "refs/heads/main", sha1st))
	if resp2.StatusCode != 202 && resp2.StatusCode != 200 {
		t.Errorf("second push = %d", resp2.StatusCode)
	}
	if app.mintedCount() > first+1 {
		t.Errorf("a fresh installation token was not reused (%d minted)", app.mintedCount())
	}

	// Removing the integration deletes the private key and the webhook stops answering.
	admin.expect(204, "DELETE", "/v1/integrations/github", nil)
	if _, ok := h.srv.internalSecretValue(t.Context(), ghAppKeySecret); ok {
		t.Error("the app's private key survived removal")
	}
	if resp, _ := h.signedWebhook(appWebhookSec, "ping", "app-3", map[string]string{}); resp.StatusCode != 404 {
		t.Errorf("after removal webhook = %d, want 404", resp.StatusCode)
	}
}

func TestInstallationTokenIsRefreshedBeforeItExpires(t *testing.T) {
	gh := newFakeGitHub(t)
	app := newFakeGHApp(t, gh)
	app.ttl = 2 * time.Minute // inside the five minute margin: every use must mint a new one
	h := newHarness(t, &executor.Fake{})
	admin := h.adminBrowser("ada")
	state, _ := startApp(t, admin, app.srv.URL, nil)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(h.http.URL + "/v1/integrations/github/app/callback?code=" + appGoodCode + "&state=" + state)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	admin.expect(200, "POST", "/v1/integrations/github/app/install", map[string]any{"installation_id": appInstall, "public_url": "https://ci.example.com",
		"rules": []map[string]any{{"repo": ghRepo, "branches": []string{"main"}}}})
	before := app.mintedCount()
	for i := 0; i < 3; i++ {
		admin.expect(200, "GET", fmt.Sprintf("/v1/integrations/github/app/repos?installation_id=%d", appInstall), nil)
		admin.expect(200, "POST", "/v1/integrations/github/test", nil)
	}
	if app.mintedCount() < before+3 {
		t.Errorf("short-lived tokens were reused past the margin: %d -> %d", before, app.mintedCount())
	}
}

func TestGitHubWebBase(t *testing.T) {
	for in, want := range map[string]string{
		"": "https://github.com", "https://api.github.com": "https://github.com",
		"https://ghe.example.com/api/v3": "https://ghe.example.com", "http://127.0.0.1:9911": "http://127.0.0.1:9911",
	} {
		if got := githubWebBase(in); got != want {
			t.Errorf("githubWebBase(%q) = %q, want %q", in, got, want)
		}
	}
}

var _ = io.EOF

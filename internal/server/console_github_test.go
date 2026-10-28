package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/forgelinehq/forgeline/internal/authn"
	"github.com/forgelinehq/forgeline/internal/docstore"
	"github.com/forgelinehq/forgeline/internal/executor"
	"github.com/forgelinehq/forgeline/internal/store"
	"github.com/forgelinehq/forgeline/internal/vault"
)

// connectGitHub configures the integration through the API the way the console does.
func connectGitHub(t *testing.T, admin *browser, gh *fakeGitHub, extra map[string]any) GitHubJSON {
	t.Helper()
	req := map[string]any{
		"enabled": true, "api_base": gh.srv.URL, "public_url": "https://ci.example.com",
		"rules": []map[string]any{{"repo": ghRepo, "branches": []string{"main"}}},
		"token": ghToken, "generate_webhook_secret": true,
	}
	for k, v := range extra {
		req[k] = v
	}
	var out GitHubJSON
	json.Unmarshal(admin.expect(200, "PUT", "/v1/integrations/github", req), &out)
	return out
}

func (h *harness) signedWebhook(secret, event, delivery string, payload any) (*http.Response, string) {
	h.t.Helper()
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", h.http.URL+"/webhooks/github", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestConsoleConfiguresGitHubEndToEnd(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addCommit(ghRepo, sha1st, map[string]string{"forgeline.yaml": ghPipeline})
	h := newHarness(t, &executor.Fake{})
	admin := h.adminBrowser("ada")

	// Before configuration the webhook endpoint exists but is off.
	resp, _ := h.signedWebhook("whatever-secret-value", "ping", "d0", map[string]string{})
	if resp.StatusCode != 404 {
		t.Fatalf("unconfigured webhook = %d, want 404", resp.StatusCode)
	}

	out := connectGitHub(t, admin, gh, nil)
	if !out.Active || out.Source != "console" || !out.TokenSet || !out.WebhookSecretSet {
		t.Fatalf("after connect: %+v", out)
	}
	secret := out.GeneratedWebhookSecret
	if len(secret) < 32 {
		t.Fatalf("generated webhook secret = %q", secret)
	}
	if out.WebhookURL != "https://ci.example.com/webhooks/github" {
		t.Errorf("webhook url = %q", out.WebhookURL)
	}

	// Neither the token nor the secret can be read back, from any endpoint or the audit log.
	for _, path := range []string{"/v1/integrations/github", "/v1/secrets", "/v1/audit", "/v1/users", "/v1/overview"} {
		body := string(admin.expect(200, "GET", path, nil))
		if strings.Contains(body, ghToken) || strings.Contains(body, secret) {
			t.Errorf("GET %s exposes a credential", path)
		}
	}

	// A signed ping works; a wrong signature does not.
	if resp, _ := h.signedWebhook(secret, "ping", "d1", map[string]string{}); resp.StatusCode != 200 {
		t.Errorf("signed ping = %d", resp.StatusCode)
	}
	if resp, _ := h.signedWebhook("wrong-secret-wrong-secret", "ping", "d2", map[string]string{}); resp.StatusCode != 401 {
		t.Errorf("badly signed ping = %d, want 401", resp.StatusCode)
	}

	// A push starts a run, posts a status, is attributed to github and audited.
	resp, body := h.signedWebhook(secret, "push", "d3", pushEvent(ghRepo, "refs/heads/main", sha1st))
	if resp.StatusCode != 202 {
		t.Fatalf("push = %d %s", resp.StatusCode, body)
	}
	gh.waitStates(h, sha1st, "pending", "success")
	runs := h.runs()
	if len(runs) != 1 || runs[0].Trigger != store.TriggerGitHub {
		t.Fatalf("runs = %+v", runs)
	}
	var view GitHubJSON
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/github", nil), &view)
	outcomes := map[string]bool{}
	for _, d := range view.Deliveries {
		outcomes[d.Outcome] = true
	}
	for _, want := range []string{"ok", "rejected", "run started"} {
		if !outcomes[want] {
			t.Errorf("deliveries lack outcome %q: %+v", want, view.Deliveries)
		}
	}
	var audit struct{ Events []AuditEvent }
	json.Unmarshal(admin.expect(200, "GET", "/v1/audit", nil), &audit)
	seen := map[string]string{}
	for _, e := range audit.Events {
		seen[e.Action] = e.Actor
	}
	if seen["integration.github.update"] != "ada" || seen["run.start"] != "github" {
		t.Errorf("audit actions = %v", seen)
	}
	var run RunJSON
	json.Unmarshal(admin.expect(200, "GET", "/v1/runs/"+runs[0].ID, nil), &run)
	if run.TriggeredBy != "github" || run.Source == nil || run.Source.SHA != sha1st {
		t.Errorf("run = %+v", run)
	}

	// Disabling turns the endpoint off at once; the stored credentials are kept.
	connectGitHub(t, admin, gh, map[string]any{"enabled": false, "token": "", "generate_webhook_secret": false})
	if resp, _ := h.signedWebhook(secret, "ping", "d4", map[string]string{}); resp.StatusCode != 404 {
		t.Errorf("disabled webhook = %d, want 404", resp.StatusCode)
	}
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/github", nil), &view)
	if view.Active || view.Enabled || !view.TokenSet || !view.WebhookSecretSet {
		t.Errorf("disabled view = %+v", view)
	}
	// Enabling again without retyping anything works, with the secret unchanged.
	connectGitHub(t, admin, gh, map[string]any{"token": "", "generate_webhook_secret": false})
	if resp, _ := h.signedWebhook(secret, "ping", "d5", map[string]string{}); resp.StatusCode != 200 {
		t.Errorf("re-enabled webhook = %d, want 200", resp.StatusCode)
	}

	admin.expect(204, "DELETE", "/v1/integrations/github", nil)
	if resp, _ := h.signedWebhook(secret, "ping", "d6", map[string]string{}); resp.StatusCode != 404 {
		t.Errorf("deleted integration still answers: %d", resp.StatusCode)
	}
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/github", nil), &view)
	if view.Configured || view.TokenSet || view.WebhookSecretSet {
		t.Errorf("after delete: %+v", view)
	}
}

func TestGitHubSettingsAreValidatedBeforeAnythingChanges(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newHarness(t, &executor.Fake{})
	admin := h.adminBrowser("ada")
	good := map[string]any{"enabled": true, "api_base": gh.srv.URL, "token": ghToken, "generate_webhook_secret": true,
		"rules": []map[string]any{{"repo": ghRepo, "branches": []string{"main"}}}}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for kk, vv := range good {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	bad := map[string]map[string]any{
		"repo name":           with("rules", []map[string]any{{"repo": "not a repo", "branches": []string{"main"}}}),
		"no branches":         with("rules", []map[string]any{{"repo": ghRepo, "branches": []string{}}}),
		"no rules":            with("rules", []map[string]any{}),
		"bad branch pattern":  with("rules", []map[string]any{{"repo": ghRepo, "branches": []string{"[bad"}}}),
		"plain http remote":   with("api_base", "http://github.example.com"),
		"no token":            with("token", ""),
		"escaping pipeline":   with("pipeline_file", "../../etc/passwd"),
		"newline in token":    with("token", "abc\ndef"),
		"weak webhook secret": with("webhook_secret", "short"),
	}
	delete(bad["weak webhook secret"], "generate_webhook_secret")
	for name, req := range bad {
		resp, body := admin.do("PUT", "/v1/integrations/github", req)
		if resp.StatusCode != 400 {
			t.Errorf("%s: got %d %s, want 400", name, resp.StatusCode, body)
		}
	}
	// Nothing was stored by the failures.
	var view GitHubJSON
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/github", nil), &view)
	if view.Configured || view.TokenSet || view.Active {
		t.Fatalf("a rejected save left state behind: %+v", view)
	}
	if n, _ := h.srv.docs.Count(context.Background(), kindSecret); n != 0 {
		t.Fatalf("a rejected save stored %d secrets", n)
	}
}

func TestGitHubConnectionTest(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.addCommit(ghRepo, sha1st, map[string]string{"forgeline.yaml": ghPipeline})
	h := newHarness(t, &executor.Fake{})
	admin := h.adminBrowser("ada")

	admin.expect(409, "POST", "/v1/integrations/github/test", nil) // not active yet
	connectGitHub(t, admin, gh, map[string]any{"rules": []map[string]any{
		{"repo": ghRepo, "branches": []string{"main"}},
		{"repo": "acme/missing", "branches": []string{"main"}},
	}})
	var res struct {
		OK      bool
		Results []RepoCheck
	}
	json.Unmarshal(admin.expect(200, "POST", "/v1/integrations/github/test", nil), &res)
	if res.OK || len(res.Results) != 2 || !res.Results[0].OK || res.Results[1].OK || res.Results[1].Status != 404 {
		t.Fatalf("results = %+v", res)
	}

	// A wrong token is reported plainly.
	connectGitHub(t, admin, gh, map[string]any{"token": "ghp_wrong_token", "generate_webhook_secret": false})
	json.Unmarshal(admin.expect(200, "POST", "/v1/integrations/github/test", nil), &res)
	if res.OK || res.Results[0].Status != 401 || !strings.Contains(res.Results[0].Message, "rejected the token") {
		t.Fatalf("wrong token results = %+v", res)
	}
}

func TestGitHubIntegrationRolesAndPersistence(t *testing.T) {
	gh := newFakeGitHub(t)
	docs := docstore.NewMemory()
	key := bytes.Repeat([]byte{42}, vault.KeySize)
	mk := func(k []byte) *harness {
		v, _ := vault.New(k)
		return newHarness(t, &executor.Fake{}, func(c *Config) { c.Docs, c.Vault = docs, v })
	}
	h := mk(key)
	admin := h.adminBrowser("ada")
	admin.createUser("dev", authn.Developer)
	admin.createUser("view", authn.Viewer)
	dev, view := h.login("dev"), h.login("view")
	dev.expect(403, "PUT", "/v1/integrations/github", map[string]any{"enabled": false, "rules": []any{}})
	dev.expect(403, "POST", "/v1/integrations/github/test", nil)
	dev.expect(403, "DELETE", "/v1/integrations/github", nil)
	dev.expect(200, "GET", "/v1/integrations/github", nil)
	view.expect(403, "GET", "/v1/integrations/github", nil)

	out := connectGitHub(t, admin, gh, nil)
	secret := out.GeneratedWebhookSecret

	// A new server on the same stored data comes up with the integration active.
	h2 := mk(key)
	if resp, _ := h2.signedWebhook(secret, "ping", "p1", map[string]string{}); resp.StatusCode != 200 {
		t.Fatalf("after restart, signed ping = %d, want 200", resp.StatusCode)
	}

	// With a different master key the credentials cannot be opened: the integration stays off
	// and the console says why, instead of reporting a confusing validation error.
	h3 := mk(bytes.Repeat([]byte{7}, vault.KeySize))
	h3.login("ada").expect(200, "GET", "/v1/integrations/github", nil)
	var v GitHubJSON
	json.Unmarshal(h3.login("ada").expect(200, "GET", "/v1/integrations/github", nil), &v)
	if v.Active || !strings.Contains(v.Problem, "master key") {
		t.Fatalf("with the wrong key: %+v", v)
	}
	if resp, _ := h3.signedWebhook(secret, "ping", "p2", map[string]string{}); resp.StatusCode != 404 {
		t.Fatalf("with the wrong key the webhook answers %d, want 404", resp.StatusCode)
	}
}

func TestCommandLineGitHubConfigStillWorksAndShowsAsFlags(t *testing.T) {
	gh := newFakeGitHub(t)
	h := ghHarness(t, gh, &executor.Fake{})
	admin := h.adminBrowser("ada")
	var view GitHubJSON
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/github", nil), &view)
	if view.Source != "flags" || !view.Active || len(view.Rules) != 1 || view.Rules[0].Repo != ghRepo {
		t.Fatalf("view = %+v", view)
	}
	// The console can take over; deleting its settings leaves it off, not back on the flags.
	connectGitHub(t, admin, gh, nil)
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/github", nil), &view)
	if view.Source != "console" {
		t.Fatalf("after connect, source = %q", view.Source)
	}
}

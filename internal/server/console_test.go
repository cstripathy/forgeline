package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/cstripathy/forgeline/internal/authn"
	"github.com/cstripathy/forgeline/internal/executor"
)

func init() {
	authn.Iterations = 2000 // the real work factor is for production; tests check behaviour
}

const pw = "a-fine-long-password"

// browser is an HTTP client with a cookie jar that sends the headers the UI sends.
type browser struct {
	t   *testing.T
	h   *harness
	c   *http.Client
	csr bool // send the CSRF header
}

func (h *harness) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: h.t, h: h, c: &http.Client{Jar: jar}, csr: true}
}

func (b *browser) do(method, path string, body any) (*http.Response, []byte) {
	b.t.Helper()
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, b.h.http.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.csr {
		req.Header.Set(csrfHeader, "1")
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func (b *browser) expect(want int, method, path string, body any) []byte {
	b.t.Helper()
	resp, out := b.do(method, path, body)
	if resp.StatusCode != want {
		b.t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, want, out)
	}
	return out
}

// setup creates the first admin through the real endpoint and returns a signed-in browser.
func (h *harness) adminBrowser(username string) *browser {
	h.t.Helper()
	b := h.browser()
	b.expect(201, "POST", "/v1/auth/setup", map[string]string{"token": testToken, "username": username, "password": pw})
	return b
}

func (b *browser) createUser(username string, role authn.Role) {
	b.t.Helper()
	b.expect(201, "POST", "/v1/users", map[string]any{"username": username, "password": pw, "role": role})
}

func (h *harness) login(username string) *browser {
	h.t.Helper()
	b := h.browser()
	b.expect(200, "POST", "/v1/auth/login", map[string]string{"username": username, "password": pw})
	return b
}

func newConsoleHarness(t *testing.T) *harness { return newHarness(t, &executor.Fake{}) }

func TestSetupNeedsServerTokenAndHappensOnce(t *testing.T) {
	h := newConsoleHarness(t)
	b := h.browser()

	var st authStateJSON
	json.Unmarshal(b.expect(200, "GET", "/v1/auth/state", nil), &st)
	if !st.SetupNeeded || st.Authenticated {
		t.Fatalf("fresh server state = %+v", st)
	}

	b.expect(401, "POST", "/v1/auth/setup", map[string]string{"token": "wrong-token-wrong-token", "username": "mallory", "password": pw})
	b.expect(400, "POST", "/v1/auth/setup", map[string]string{"token": testToken, "username": "ada", "password": "short"})
	b.expect(201, "POST", "/v1/auth/setup", map[string]string{"token": testToken, "username": "ada", "name": "Ada L", "password": pw})
	b.expect(409, "POST", "/v1/auth/setup", map[string]string{"token": testToken, "username": "eve", "password": pw})

	// The setup call signed Ada in.
	json.Unmarshal(b.expect(200, "GET", "/v1/auth/state", nil), &st)
	if st.SetupNeeded || !st.Authenticated || st.User == nil || st.User.Username != "ada" || st.User.Role != authn.Admin {
		t.Fatalf("state after setup = %+v", st)
	}
}

func TestPasswordHashNeverLeavesTheServer(t *testing.T) {
	h := newConsoleHarness(t)
	b := h.adminBrowser("ada")
	for _, path := range []string{"/v1/users", "/v1/auth/state", "/v1/audit"} {
		if out := b.expect(200, "GET", path, nil); strings.Contains(string(out), "pbkdf2") || strings.Contains(string(out), pw) {
			t.Errorf("%s leaks credentials: %s", path, out)
		}
	}
}

func TestLoginWrongPasswordAndUnknownUserLookIdentical(t *testing.T) {
	h := newConsoleHarness(t)
	h.adminBrowser("ada")
	b := h.browser()
	_, a := b.do("POST", "/v1/auth/login", map[string]string{"username": "ada", "password": "not-the-password"})
	_, c := b.do("POST", "/v1/auth/login", map[string]string{"username": "nobody", "password": "not-the-password"})
	if string(a) != string(c) {
		t.Fatalf("responses differ, which reveals which accounts exist:\n%s\n%s", a, c)
	}
}

func TestDisabledAccountLooksLikeAWrongPassword(t *testing.T) {
	h := newConsoleHarness(t)
	admin := h.adminBrowser("ada")
	admin.createUser("dev", authn.Developer)
	admin.expect(200, "PATCH", "/v1/users/dev", map[string]any{"disabled": true})
	b := h.browser()
	_, disabled := b.do("POST", "/v1/auth/login", map[string]string{"username": "dev", "password": pw})
	_, wrong := b.do("POST", "/v1/auth/login", map[string]string{"username": "dev", "password": "not-the-password"})
	if string(disabled) != string(wrong) {
		t.Fatalf("a disabled account is distinguishable by its error:\n%s\n%s", disabled, wrong)
	}
}

func TestLoginIsThrottled(t *testing.T) {
	h := newConsoleHarness(t)
	h.adminBrowser("ada")
	b := h.browser()
	for i := 0; i < maxLoginFails; i++ {
		b.expect(401, "POST", "/v1/auth/login", map[string]string{"username": "ada", "password": "wrong-wrong-wrong"})
	}
	resp, _ := b.do("POST", "/v1/auth/login", map[string]string{"username": "ada", "password": pw})
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("after %d failures the right password got %d, want 429 with Retry-After", maxLoginFails, resp.StatusCode)
	}
}

func TestRolesGateEveryAction(t *testing.T) {
	h := newConsoleHarness(t)
	admin := h.adminBrowser("ada")
	admin.createUser("dev", authn.Developer)
	admin.createUser("view", authn.Viewer)
	dev, view := h.login("dev"), h.login("view")

	body, ct := multipartBody(t, twoSteps, nil)
	submit := func(b *browser) int {
		req, _ := http.NewRequest("POST", h.http.URL+"/v1/runs", body)
		req.Header.Set("Content-Type", ct)
		req.Header.Set(csrfHeader, "1")
		resp, err := b.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := submit(view); code != 403 {
		t.Errorf("viewer submitting a run = %d, want 403", code)
	}
	body, ct = multipartBody(t, twoSteps, nil)
	if code := submit(dev); code != 202 {
		t.Errorf("developer submitting a run = %d, want 202", code)
	}

	// Everyone signed in can read runs; only admins reach users and the audit log.
	for name, b := range map[string]*browser{"viewer": view, "developer": dev} {
		b.expect(200, "GET", "/v1/runs", nil)
		b.expect(403, "GET", "/v1/users", nil)
		b.expect(403, "GET", "/v1/audit", nil)
		b.expect(403, "POST", "/v1/users", map[string]any{"username": "x-" + name, "password": pw, "role": "admin"})
	}
	admin.expect(200, "GET", "/v1/users", nil)
	// Secret names are for people who build pipelines, not for viewers.
	view.expect(403, "GET", "/v1/secrets", nil)
	dev.expect(200, "GET", "/v1/secrets", nil)

	// Refusals are in the audit log.
	var out struct{ Events []AuditEvent }
	json.Unmarshal(admin.expect(200, "GET", "/v1/audit?denied=1", nil), &out)
	denied := 0
	for _, e := range out.Events {
		if e.Action == "access.denied" && (e.Actor == "view" || e.Actor == "dev") {
			denied++
		}
	}
	if denied < 7 {
		t.Errorf("audit log has %d access.denied events for the refused calls, want at least 7: %+v", denied, out.Events)
	}
}

func TestUnauthenticatedCallsAreRefused(t *testing.T) {
	h := newConsoleHarness(t)
	h.adminBrowser("ada")
	anon := h.browser()
	for _, p := range []string{"/v1/runs", "/v1/users", "/v1/audit"} {
		anon.expect(401, "GET", p, nil)
	}
	// The bearer token still works, as an admin named "token", for the CLI.
	resp, _ := h.do("GET", "/v1/users", testToken, nil, "")
	if resp.StatusCode != 200 {
		t.Fatalf("bearer token on /v1/users = %d, want 200", resp.StatusCode)
	}
	resp, _ = h.do("GET", "/v1/users", "wrong-token-wrong-token", nil, "")
	if resp.StatusCode != 401 {
		t.Fatalf("wrong bearer token = %d, want 401", resp.StatusCode)
	}
}

func TestCookieRequestsNeedTheCSRFHeaderAndSameOrigin(t *testing.T) {
	h := newConsoleHarness(t)
	admin := h.adminBrowser("ada")

	admin.csr = false
	resp, _ := admin.do("POST", "/v1/users", map[string]any{"username": "eve", "password": pw, "role": "viewer"})
	if resp.StatusCode != 403 {
		t.Fatalf("state change without the CSRF header = %d, want 403", resp.StatusCode)
	}
	admin.csr = true

	req, _ := http.NewRequest("POST", h.http.URL+"/v1/users", strings.NewReader(`{"username":"eve","password":"`+pw+`","role":"viewer"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, "1")
	req.Header.Set("Origin", "https://evil.example")
	r, err := admin.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 403 {
		t.Fatalf("cross-origin state change = %d, want 403", r.StatusCode)
	}

	// A form post (text/plain) cannot carry JSON to the API.
	req, _ = http.NewRequest("POST", h.http.URL+"/v1/auth/login", strings.NewReader(`{"username":"ada","password":"`+pw+`"}`))
	req.Header.Set("Content-Type", "text/plain")
	r, _ = http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != 415 {
		t.Fatalf("text/plain login = %d, want 415", r.StatusCode)
	}

	// Reading needs no header: GETs are safe.
	admin.csr = false
	admin.expect(200, "GET", "/v1/users", nil)
}

func TestSessionCookieAttributes(t *testing.T) {
	h := newConsoleHarness(t)
	req, _ := http.NewRequest("POST", h.http.URL+"/v1/auth/setup", strings.NewReader(`{"token":"`+testToken+`","username":"ada","password":"`+pw+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var c *http.Cookie
	for _, k := range resp.Cookies() {
		if k.Name == sessionCookie {
			c = k
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || !c.Secure || c.Path != "/" || c.MaxAge <= 0 {
		t.Fatalf("session cookie = %+v", c)
	}
	if len(c.Value) < 40 {
		t.Fatalf("session token is only %d characters", len(c.Value))
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	h := newConsoleHarness(t)
	b := h.adminBrowser("ada")
	b.expect(200, "GET", "/v1/runs", nil)
	b.expect(204, "POST", "/v1/auth/logout", nil)
	b.expect(401, "GET", "/v1/runs", nil)
}

func TestLastAdminCannotBeRemoved(t *testing.T) {
	h := newConsoleHarness(t)
	admin := h.adminBrowser("ada")
	admin.expect(409, "DELETE", "/v1/users/ada", nil)
	admin.expect(409, "PATCH", "/v1/users/ada", map[string]any{"role": "viewer"})
	admin.expect(409, "PATCH", "/v1/users/ada", map[string]any{"disabled": true})

	admin.createUser("grace", authn.Admin)
	admin.expect(200, "PATCH", "/v1/users/ada", map[string]any{"role": "developer"}) // fine now
	grace := h.login("grace")
	grace.expect(409, "DELETE", "/v1/users/grace", nil)
}

func TestDisablingOrResettingEndsSessions(t *testing.T) {
	h := newConsoleHarness(t)
	admin := h.adminBrowser("ada")
	admin.createUser("dev", authn.Developer)

	dev := h.login("dev")
	dev.expect(200, "GET", "/v1/runs", nil)
	admin.expect(200, "PATCH", "/v1/users/dev", map[string]any{"disabled": true})
	dev.expect(401, "GET", "/v1/runs", nil)
	h.browser().expect(401, "POST", "/v1/auth/login", map[string]string{"username": "dev", "password": pw})

	// Re-enabling must not resurrect the sessions that existed before the account was disabled.
	admin.expect(200, "PATCH", "/v1/users/dev", map[string]any{"disabled": false})
	dev.expect(401, "GET", "/v1/runs", nil)
	admin.expect(200, "PATCH", "/v1/users/dev", map[string]any{"disabled": false, "password": "another-long-password"})
	dev2 := h.browser()
	dev2.expect(200, "POST", "/v1/auth/login", map[string]string{"username": "dev", "password": "another-long-password"})
	admin.expect(200, "PATCH", "/v1/users/dev", map[string]any{"password": "third-long-password!"})
	dev2.expect(401, "GET", "/v1/runs", nil) // a reset signs the person out everywhere
}

func TestChangeOwnPassword(t *testing.T) {
	h := newConsoleHarness(t)
	admin := h.adminBrowser("ada")
	admin.createUser("dev", authn.Developer)
	dev := h.login("dev")

	dev.expect(403, "POST", "/v1/me/password", map[string]string{"current": "wrong-current-pass", "new": "a-brand-new-password"})
	dev.expect(400, "POST", "/v1/me/password", map[string]string{"current": pw, "new": "short"})
	dev.expect(204, "POST", "/v1/me/password", map[string]string{"current": pw, "new": "a-brand-new-password"})
	dev.expect(401, "GET", "/v1/runs", nil)
	h.browser().expect(200, "POST", "/v1/auth/login", map[string]string{"username": "dev", "password": "a-brand-new-password"})
}

func TestUsernamesAreUniqueIgnoringCase(t *testing.T) {
	h := newConsoleHarness(t)
	admin := h.adminBrowser("Ada")
	admin.expect(409, "POST", "/v1/users", map[string]any{"username": "ADA", "password": pw, "role": "viewer"})
	admin.expect(400, "POST", "/v1/users", map[string]any{"username": "bad name!", "password": pw, "role": "viewer"})
	admin.expect(400, "POST", "/v1/users", map[string]any{"username": "okname", "password": pw, "role": "root"})
	h.browser().expect(200, "POST", "/v1/auth/login", map[string]string{"username": "ada", "password": pw})
}

func TestAuditLogRecordsWhoDidWhat(t *testing.T) {
	h := newConsoleHarness(t)
	admin := h.adminBrowser("ada")
	admin.createUser("dev", authn.Developer)
	h.browser().do("POST", "/v1/auth/login", map[string]string{"username": "dev", "password": "wrong-wrong-wrong"})
	admin.expect(200, "PATCH", "/v1/users/dev", map[string]any{"role": "viewer"})

	var out struct{ Events []AuditEvent }
	json.Unmarshal(admin.expect(200, "GET", "/v1/audit", nil), &out)
	seen := map[string]AuditEvent{}
	for _, e := range out.Events {
		seen[e.Action] = e
	}
	for action, wantActor := range map[string]string{"setup.complete": "ada", "user.create": "ada", "login.failed": "dev", "user.update": "ada"} {
		e, ok := seen[action]
		if !ok || e.Actor != wantActor {
			t.Errorf("audit %q = %+v (present=%v), want actor %q", action, e, ok, wantActor)
		}
	}
	if e := seen["login.failed"]; e.Allowed {
		t.Error("a failed login must be recorded as not allowed")
	}
	if e := seen["user.update"]; !strings.Contains(e.Detail, "developer->viewer") {
		t.Errorf("user.update detail = %q", e.Detail)
	}
	for i := 1; i < len(out.Events); i++ {
		if out.Events[i-1].ID < out.Events[i].ID {
			t.Fatal("audit events are not newest first")
		}
	}
	admin.expect(400, "GET", "/v1/audit?limit=0", nil)
	json.Unmarshal(admin.expect(200, "GET", "/v1/audit?actor=DEV", nil), &out)
	for _, e := range out.Events {
		if strings.ToLower(e.Actor) != "dev" {
			t.Errorf("actor filter returned %+v", e)
		}
	}
}

package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cstripathy/forgeline/internal/authn"
	"github.com/cstripathy/forgeline/internal/executor"
)

const (
	ssoAccessToken = "aoaAAAAAGexample-sso-access-token-0123456789abcdef"
	ssoSecretKey   = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

// fakeSSO stands in for the AWS OIDC and portal endpoints.
type fakeSSO struct {
	srv *httptest.Server

	mu          sync.Mutex
	approved    bool
	denied      bool
	tokenCalls  int
	credCalls   int
	credTTL     time.Duration
	badLink     string
	bearerSeen  []string
	registered  map[string]any
	deviceStart map[string]any
}

func newFakeSSO(t *testing.T) *fakeSSO {
	f := &fakeSSO{credTTL: time.Hour}
	mux := http.NewServeMux()
	body := func(r *http.Request) map[string]any {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		return m
	}
	reply := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /oidc/client/register", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.registered = body(r)
		f.mu.Unlock()
		reply(w, 200, map[string]any{"clientId": "cid", "clientSecret": "csecret"})
	})
	mux.HandleFunc("POST /oidc/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		m := body(r)
		f.mu.Lock()
		f.deviceStart = m
		f.mu.Unlock()
		f.mu.Lock()
		link := f.srv.URL + "/verify?user_code=ABCD-EFGH"
		if f.badLink != "" {
			link = f.badLink
		}
		f.mu.Unlock()
		reply(w, 200, map[string]any{"deviceCode": "dev-code", "userCode": "ABCD-EFGH", "verificationUri": link,
			"verificationUriComplete": link, "expiresIn": 600, "interval": 1})
	})
	mux.HandleFunc("POST /oidc/token", func(w http.ResponseWriter, r *http.Request) {
		m := body(r)
		f.mu.Lock()
		f.tokenCalls++
		approved, denied := f.approved, f.denied
		f.mu.Unlock()
		switch {
		case m["deviceCode"] != "dev-code" || m["clientSecret"] != "csecret" || m["grantType"] != ssoDeviceGrant:
			reply(w, 400, map[string]any{"error": "invalid_grant"})
		case denied:
			reply(w, 400, map[string]any{"error": "access_denied"})
		case !approved:
			reply(w, 400, map[string]any{"error": "authorization_pending"})
		default:
			reply(w, 200, map[string]any{"accessToken": ssoAccessToken, "tokenType": "Bearer", "expiresIn": 28800})
		}
	})
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		got := r.Header.Get("x-amz-sso_bearer_token")
		f.mu.Lock()
		f.bearerSeen = append(f.bearerSeen, got)
		f.mu.Unlock()
		if got != ssoAccessToken {
			reply(w, 401, map[string]any{"message": "Session token not found or invalid"})
			return false
		}
		return true
	}
	mux.HandleFunc("GET /portal/assignment/accounts", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			reply(w, 200, map[string]any{"accountList": []map[string]string{
				{"accountId": "222233334444", "accountName": "zeta-sandbox", "emailAddress": "z@example.com"},
				{"accountId": "111122223333", "accountName": "alpha-prod", "emailAddress": "a@example.com"}}})
		}
	})
	mux.HandleFunc("GET /portal/assignment/roles", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			reply(w, 200, map[string]any{"roleList": []map[string]string{{"roleName": "ReadOnly"}, {"roleName": "Admin"}}})
		}
	})
	mux.HandleFunc("GET /portal/federation/credentials", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		f.mu.Lock()
		f.credCalls++
		ttl := f.credTTL
		f.mu.Unlock()
		reply(w, 200, map[string]any{"roleCredentials": map[string]any{"accessKeyId": "ASIAEXAMPLE", "secretAccessKey": ssoSecretKey,
			"sessionToken": "session-token-example", "expiration": time.Now().Add(ttl).UnixMilli()}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSSO) set(fn func()) { f.mu.Lock(); fn(); f.mu.Unlock() }

// awsSpy records how the server ran the aws command.
type awsSpy struct {
	mu   sync.Mutex
	envs [][]string
	args [][]string
}

func (a *awsSpy) wrap(next func(context.Context, string, ...string) ([]byte, string, error)) cmdRunner {
	return func(ctx context.Context, name string, args ...string) ([]byte, string, error) {
		if name == "aws" {
			env, _ := ctx.Value(extraEnvKey{}).([]string)
			a.mu.Lock()
			a.envs, a.args = append(a.envs, env), append(a.args, args)
			a.mu.Unlock()
		}
		return next(ctx, name, args...)
	}
}

func TestConsoleSignsInToAWSWithSSO(t *testing.T) {
	sso := newFakeSSO(t)
	h := newHarness(t, &executor.Fake{}, func(c *Config) { c.SSOBase = sso.srv.URL })
	cli, spy := newFakeCLI(), &awsSpy{}
	h.srv.run = spy.wrap(cli.run)
	admin := h.adminBrowser("ada")
	admin.createUser("dev", authn.Developer)
	dev := h.login("dev")

	var st SSOStatusJSON
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/aws/sso", nil), &st)
	if st.SignedIn || st.Active {
		t.Fatalf("fresh status = %+v", st)
	}

	// Only administrators sign in; the inputs are checked before AWS is contacted.
	dev.expect(403, "POST", "/v1/integrations/aws/sso/start", map[string]any{"start_url": "https://d-123.awsapps.com/start", "sso_region": "ap-south-1"})
	admin.expect(400, "POST", "/v1/integrations/aws/sso/start", map[string]any{"start_url": "http://d-123.awsapps.com/start", "sso_region": "ap-south-1"})
	admin.expect(400, "POST", "/v1/integrations/aws/sso/start", map[string]any{"start_url": "https://evil.example.com/start", "sso_region": "ap-south-1"})
	admin.expect(400, "POST", "/v1/integrations/aws/sso/start", map[string]any{"start_url": "https://d-123.awsapps.com/start?x=1", "sso_region": "ap-south-1"})
	admin.expect(400, "POST", "/v1/integrations/aws/sso/start", map[string]any{"start_url": "https://d-123.awsapps.com/start", "sso_region": "mars"})

	// Start: the person gets a code and a link.
	var start SSOStartJSON
	json.Unmarshal(admin.expect(200, "POST", "/v1/integrations/aws/sso/start", map[string]any{"start_url": "https://d-123.awsapps.com/start", "sso_region": "ap-south-1"}), &start)
	if start.UserCode != "ABCD-EFGH" || !strings.Contains(start.VerificationURL, "user_code=ABCD-EFGH") || start.LoginID == "" {
		t.Fatalf("start = %+v", start)
	}
	if sso.deviceStart["startUrl"] != "https://d-123.awsapps.com/start" || sso.registered["clientName"] != "forgeline" {
		t.Errorf("AWS was sent %v / %v", sso.registered, sso.deviceStart)
	}

	poll := func(id string) string {
		var out map[string]string
		json.Unmarshal(admin.expect(200, "POST", "/v1/integrations/aws/sso/poll", map[string]any{"login_id": id}), &out)
		return out["status"]
	}
	// Not approved yet. Polling again at once does not call AWS again (it asked for a pause).
	if got := poll(start.LoginID); got != "pending" {
		t.Fatalf("poll = %q, want pending", got)
	}
	if got := poll(start.LoginID); got != "pending" || sso.tokenCalls != 1 {
		t.Fatalf("second poll = %q after %d token calls, want pending after 1", got, sso.tokenCalls)
	}
	// Another administrator cannot finish someone else's sign-in, and unknown ids are expired.
	admin.createUser("grace", authn.Admin)
	var other map[string]string
	json.Unmarshal(h.login("grace").expect(200, "POST", "/v1/integrations/aws/sso/poll", map[string]any{"login_id": start.LoginID}), &other)
	if other["status"] != "expired" {
		t.Errorf("another person's poll = %q, want expired", other["status"])
	}
	if got := poll("nope"); got != "expired" {
		t.Errorf("unknown id = %q", got)
	}
	admin.expect(409, "GET", "/v1/integrations/aws/sso/accounts", nil) // not signed in yet

	sso.set(func() { sso.approved = true })
	h.srv.sso.mu.Lock()
	h.srv.sso.logins[start.LoginID].interval = 0
	h.srv.sso.mu.Unlock()
	if got := poll(start.LoginID); got != "authorized" {
		t.Fatalf("after approval poll = %q", got)
	}
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/aws/sso", nil), &st)
	if !st.SignedIn || st.StartURL != "https://d-123.awsapps.com/start" || st.AccountID != "" {
		t.Fatalf("status after sign-in = %+v", st)
	}

	// Accounts and roles come from the portal with the stored token, sorted by name.
	var accts struct{ Accounts []SSOAccountJSON }
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/aws/sso/accounts", nil), &accts)
	if len(accts.Accounts) != 2 || accts.Accounts[0].Name != "alpha-prod" {
		t.Fatalf("accounts = %+v", accts.Accounts)
	}
	var roles struct{ Roles []string }
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/aws/sso/roles?account_id=111122223333", nil), &roles)
	if len(roles.Roles) != 2 || roles.Roles[0] != "Admin" {
		t.Fatalf("roles = %+v", roles.Roles)
	}
	admin.expect(400, "GET", "/v1/integrations/aws/sso/roles?account_id=12", nil)

	// Connecting proves the role works with a real identity call, then saves it.
	admin.expect(400, "POST", "/v1/integrations/aws/sso/connect", map[string]any{"account_id": "111122223333", "role_name": "bad role!", "region": "ap-south-1"})
	var res AWSSettingsResult
	json.Unmarshal(admin.expect(200, "POST", "/v1/integrations/aws/sso/connect", map[string]any{"account_id": "111122223333", "account_name": "alpha-prod", "role_name": "ReadOnly", "region": "ap-south-1"}), &res)
	if res.Identity == nil || res.Identity.Account != "111122223333" || res.Profile != ssoProfile {
		t.Fatalf("connect = %+v", res)
	}
	spy.mu.Lock()
	lastEnv, lastArgs := spy.envs[len(spy.envs)-1], strings.Join(spy.args[len(spy.args)-1], " ")
	spy.mu.Unlock()
	joined := strings.Join(lastEnv, "\n")
	if !strings.Contains(joined, "AWS_ACCESS_KEY_ID=ASIAEXAMPLE") || !strings.Contains(joined, "AWS_SESSION_TOKEN=session-token-example") || !strings.Contains(joined, "AWS_PROFILE=\n") {
		t.Errorf("aws ran without the role credentials: %q", lastEnv)
	}
	if strings.Contains(lastArgs, "--profile") || strings.Contains(lastArgs, ssoAccessToken) || strings.Contains(lastArgs, ssoSecretKey) {
		t.Errorf("aws was started with a profile or a credential on its command line: %s", lastArgs)
	}

	// The infrastructure page and settings now work through the sign-in, and the role
	// credentials were fetched once and reused.
	admin.expect(200, "GET", "/v1/infra/aws", nil)
	if sso.credCalls != 1 {
		t.Errorf("role credentials fetched %d times, want 1 (cached)", sso.credCalls)
	}
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/aws/sso", nil), &st)
	if !st.Active || st.AccountID != "111122223333" || st.RoleName != "ReadOnly" {
		t.Fatalf("status after connect = %+v", st)
	}

	// Neither the access token nor the role secret appears in any answer or the audit log.
	for _, path := range []string{"/v1/integrations/aws/sso", "/v1/integrations/aws", "/v1/infra/aws", "/v1/secrets", "/v1/audit", "/v1/overview", "/v1/users"} {
		_, b := admin.do("GET", path, nil)
		if strings.Contains(string(b), ssoAccessToken) || strings.Contains(string(b), ssoSecretKey) {
			t.Errorf("GET %s exposes an AWS credential", path)
		}
	}
	if audit := string(admin.expect(200, "GET", "/v1/audit", nil)); !strings.Contains(audit, "integration.aws.sso.signin") || !strings.Contains(audit, "integration.aws.sso.start") {
		t.Error("the sign-in was not audited")
	}
	// A real profile cannot take the reserved name.
	admin.expect(400, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": ssoProfile, "region": "ap-south-1"})

	// When the stored sign-in expires, AWS pages say so instead of using stale access.
	old, _ := h.srv.loadSSOToken(context.Background())
	old.ExpiresAt = time.Now().Add(-time.Minute)
	val, _ := json.Marshal(old)
	h.srv.putSecret(context.Background(), "test", ssoTokenName, string(val), "")
	h.srv.dropSSOCreds()
	h.srv.awsCache.mu.Lock()
	h.srv.awsCache.key = ""
	h.srv.awsCache.mu.Unlock()
	body := string(admin.expect(200, "GET", "/v1/infra/aws", nil))
	if !strings.Contains(body, "sign-in expired") {
		t.Errorf("an expired sign-in is not reported: %s", body)
	}

	// Signing out forgets the token and turns the connection off.
	admin.expect(200, "DELETE", "/v1/integrations/aws/sso", nil)
	json.Unmarshal(admin.expect(200, "GET", "/v1/integrations/aws/sso", nil), &st)
	if st.SignedIn || st.Active {
		t.Errorf("after sign-out = %+v", st)
	}
	if _, ok := h.srv.loadSSOToken(context.Background()); ok {
		t.Error("the token survived sign-out")
	}
}

func TestSSORoleCredentialsAreRefreshedBeforeTheyExpire(t *testing.T) {
	sso := newFakeSSO(t)
	sso.set(func() { sso.credTTL = 2 * time.Minute }) // inside the refresh margin
	h := newHarness(t, &executor.Fake{}, func(c *Config) { c.SSOBase = sso.srv.URL })
	h.srv.run = newFakeCLI().run
	admin := h.adminBrowser("ada")
	var start SSOStartJSON
	json.Unmarshal(admin.expect(200, "POST", "/v1/integrations/aws/sso/start", map[string]any{"start_url": "https://d-123.awsapps.com/start", "sso_region": "ap-south-1"}), &start)
	sso.set(func() { sso.approved = true })
	h.srv.sso.mu.Lock()
	h.srv.sso.logins[start.LoginID].interval = 0
	h.srv.sso.mu.Unlock()
	admin.expect(200, "POST", "/v1/integrations/aws/sso/poll", map[string]any{"login_id": start.LoginID})
	admin.expect(200, "POST", "/v1/integrations/aws/sso/connect", map[string]any{"account_id": "111122223333", "role_name": "ReadOnly", "region": "ap-south-1"})
	before := sso.credCalls
	for i := 0; i < 3; i++ {
		h.srv.dropAWSCacheForTest()
		admin.expect(200, "GET", "/v1/infra/aws", nil)
	}
	if sso.credCalls <= before {
		t.Errorf("short-lived credentials were reused past the refresh margin (%d calls)", sso.credCalls)
	}
}

func TestSSODenialAndExpiryAreReported(t *testing.T) {
	sso := newFakeSSO(t)
	h := newHarness(t, &executor.Fake{}, func(c *Config) { c.SSOBase = sso.srv.URL })
	admin := h.adminBrowser("ada")
	var start SSOStartJSON
	json.Unmarshal(admin.expect(200, "POST", "/v1/integrations/aws/sso/start", map[string]any{"start_url": "https://d-123.awsapps.com/start", "sso_region": "ap-south-1"}), &start)
	sso.set(func() { sso.denied = true })
	h.srv.sso.mu.Lock()
	h.srv.sso.logins[start.LoginID].interval = 0
	h.srv.sso.mu.Unlock()
	var out map[string]string
	json.Unmarshal(admin.expect(200, "POST", "/v1/integrations/aws/sso/poll", map[string]any{"login_id": start.LoginID}), &out)
	if out["status"] != "denied" {
		t.Fatalf("status = %q, want denied", out["status"])
	}
	// The pending sign-in is gone, so it cannot be retried with the same id.
	json.Unmarshal(admin.expect(200, "POST", "/v1/integrations/aws/sso/poll", map[string]any{"login_id": start.LoginID}), &out)
	if out["status"] != "expired" {
		t.Errorf("after denial = %q, want expired", out["status"])
	}
	if !strings.Contains(string(admin.expect(200, "GET", "/v1/audit", nil)), "integration.aws.sso.denied") {
		t.Error("the denial was not audited")
	}
}

func TestSSOStartRefusesALinkThatIsNotAWebAddress(t *testing.T) {
	sso := newFakeSSO(t)
	sso.set(func() { sso.badLink = "javascript:alert(document.cookie)" })
	h := newHarness(t, &executor.Fake{}, func(c *Config) { c.SSOBase = sso.srv.URL })
	admin := h.adminBrowser("ada")
	body := admin.expect(502, "POST", "/v1/integrations/aws/sso/start", map[string]any{"start_url": "https://d-123.awsapps.com/start", "sso_region": "ap-south-1"})
	if strings.Contains(string(body), "javascript:") {
		t.Errorf("the bad link was passed on: %s", body)
	}
}

func TestSSOVerificationLinkMustBeAWebAddress(t *testing.T) {
	for _, bad := range []string{"javascript:alert(1)", "data:text/html,x", "ftp://x/y", "//evil.example.com/x", ""} {
		if checkVerificationURL(bad) == nil {
			t.Errorf("%q was accepted as a sign-in link", bad)
		}
	}
	if err := checkVerificationURL("https://device.sso.ap-south-1.amazonaws.com/?user_code=A"); err != nil {
		t.Error(err)
	}
}

func (s *Server) dropAWSCacheForTest() {
	s.awsCache.mu.Lock()
	s.awsCache.key, s.awsCache.cost = "", nil
	s.awsCache.mu.Unlock()
}

func TestMergeEnvUnsetsClearedNames(t *testing.T) {
	base := []string{"PATH=/bin", "AWS_PROFILE=old", "AWS_DEFAULT_PROFILE=old", "KEEP=1"}
	got := mergeEnv(base, []string{"AWS_ACCESS_KEY_ID=a", "AWS_PROFILE=", "AWS_DEFAULT_PROFILE="})
	joined := "\n" + strings.Join(got, "\n") + "\n"
	for _, bad := range []string{"\nAWS_PROFILE", "\nAWS_DEFAULT_PROFILE"} {
		if strings.Contains(joined, bad) {
			t.Errorf("%s is still set (even empty, the real AWS CLI fails on it): %q", bad, got)
		}
	}
	if !strings.Contains(joined, "\nAWS_ACCESS_KEY_ID=a\n") || !strings.Contains(joined, "\nKEEP=1\n") {
		t.Errorf("lost a variable: %q", got)
	}
}

// The real runner, with a stand-in `aws` that behaves like the real CLI about an empty AWS_PROFILE.
func TestExecRunnerClearedProfileIsUnset(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"${AWS_PROFILE+set}\" = set ]; then echo \"profile set\" >&2; exit 1; fi\necho \"$AWS_ACCESS_KEY_ID\"\n"
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("AWS_PROFILE", "from-the-server")
	ctx := withExtraEnv(context.Background(), []string{"AWS_ACCESS_KEY_ID=AKIA1", "AWS_PROFILE=", "AWS_DEFAULT_PROFILE="})
	out, errText, err := execRunner(ctx, "aws")
	if err != nil || strings.TrimSpace(string(out)) != "AKIA1" {
		t.Fatalf("out=%q err=%v stderr=%q", out, err, errText)
	}
}

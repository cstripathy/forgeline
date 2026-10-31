package server

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/forgelinehq/forgeline/internal/docstore"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/forgelinehq/forgeline/internal/store"
)

func (m *mcpHarness) newToken(b *browser, body map[string]any) string {
	m.t.Helper()
	var out createTokenReply
	json.Unmarshal(b.expect(201, "POST", "/v1/tokens", body), &out)
	if !strings.HasPrefix(out.Token, apiTokenPrefix) {
		m.t.Fatalf("token = %q", out.Token)
	}
	return out.Token
}

// mcpDo posts one raw message to /mcp with the given bearer token, like an MCP client.
func (m *mcpHarness) mcpDo(token string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	m.t.Helper()
	req, _ := http.NewRequest("POST", m.http.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		m.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (m *mcpHarness) rpc(token, method string, params any) rpcReply {
	m.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, out := m.mcpDo(token, body, nil)
	if resp.StatusCode != 200 {
		m.t.Fatalf("%s: %d %s", method, resp.StatusCode, out)
	}
	var r rpcReply
	json.Unmarshal(out, &r)
	return r
}

// tool calls one tool and returns its text and whether it was an error result.
func (m *mcpHarness) tool(token, name string, args any) (string, bool) {
	m.t.Helper()
	r := m.rpc(token, "tools/call", map[string]any{"name": name, "arguments": args})
	if r.Error != nil {
		m.t.Fatalf("%s: rpc error %+v", name, r.Error)
	}
	var res mcpCallResult
	json.Unmarshal(r.Result, &res)
	if len(res.Content) != 1 {
		m.t.Fatalf("%s: %+v", name, res)
	}
	return res.Content[0].Text, res.IsError
}

func (m *mcpHarness) toolNames(token string) map[string]bool {
	m.t.Helper()
	var out struct{ Tools []mcpToolDef }
	json.Unmarshal(m.rpc(token, "tools/list", nil).Result, &out)
	names := map[string]bool{}
	for _, t := range out.Tools {
		names[t.Name] = true
	}
	return names
}

func (m *mcpHarness) auditText() string {
	m.t.Helper()
	return string(m.admin.expect(200, "GET", "/v1/audit?limit=500", nil))
}

func TestAPITokensAreHashedCappedAndOwnedByOnePerson(t *testing.T) {
	m := newMCPHarness(t)
	m.newToken(m.admin, map[string]any{"name": "ada's own"})
	tok := m.newToken(m.dev, map[string]any{"name": "claude code on my laptop"})
	// Only the hash is stored.
	docs, _ := m.srv.docs.List(context.Background(), kindAPIToken, docstoreAll)
	if raw, _ := json.Marshal(docs); strings.Contains(string(raw), tok) || strings.Contains(string(raw), strings.TrimPrefix(tok, apiTokenPrefix)) {
		t.Fatalf("the token itself was stored: %s", raw)
	}
	list := string(m.dev.expect(200, "GET", "/v1/tokens", nil))
	if strings.Contains(list, tok) || !strings.Contains(list, "claude code on my laptop") || !strings.Contains(list, `"role":"developer"`) {
		t.Errorf("list = %s", list)
	}
	// Role and size rules.
	m.dev.expect(403, "POST", "/v1/tokens", map[string]any{"name": "x", "role": "admin"})
	m.v.expect(403, "POST", "/v1/tokens", map[string]any{"name": "x", "role": "developer"})
	m.dev.expect(400, "POST", "/v1/tokens", map[string]any{"name": ""})
	m.dev.expect(400, "POST", "/v1/tokens", map[string]any{"name": "x", "role": "root"})
	m.dev.expect(400, "POST", "/v1/tokens", map[string]any{"name": "x", "days": maxTokenDays + 1})
	m.dev.expect(400, "POST", "/v1/tokens", map[string]any{"name": strings.Repeat("n", 61)})
	for i := 1; i < maxTokensPerUser; i++ {
		m.dev.expect(201, "POST", "/v1/tokens", map[string]any{"name": "t", "role": "viewer"})
	}
	m.dev.expect(409, "POST", "/v1/tokens", map[string]any{"name": "one too many"})

	// Someone else's list and revoke do not reveal or touch it; an administrator can see and revoke all.
	if strings.Contains(string(m.v.expect(200, "GET", "/v1/tokens", nil)), "laptop") {
		t.Error("a viewer saw a developer's token")
	}
	if strings.Contains(string(m.dev.expect(200, "GET", "/v1/tokens?all=1", nil)), "ada's own") {
		t.Error("all=1 is for administrators")
	}
	var mine struct{ Tokens []APIToken }
	json.Unmarshal(m.dev.expect(200, "GET", "/v1/tokens", nil), &mine)
	id := mine.Tokens[len(mine.Tokens)-1].ID
	m.v.expect(404, "DELETE", "/v1/tokens/"+id, nil)
	m.dev.expect(204, "DELETE", "/v1/tokens/"+id, nil)
	m.dev.expect(404, "DELETE", "/v1/tokens/"+id, nil)
	if !strings.Contains(string(m.admin.expect(200, "GET", "/v1/tokens?all=1", nil)), `"owner":"dev"`) {
		t.Error("the administrator cannot see everyone's tokens")
	}
	if a := m.auditText(); !strings.Contains(a, "token.create") || !strings.Contains(a, "token.revoke") {
		t.Errorf("token changes are not audited: %s", a)
	}
}

func TestAPITokensCannotMakeTokensAndTheServerTokenIsNotAPerson(t *testing.T) {
	m := newMCPHarness(t)
	tok := m.newToken(m.dev, map[string]any{"name": "a"})
	for _, bearer := range []string{tok, testToken} {
		resp, body := m.h().do("POST", "/v1/tokens", bearer, strings.NewReader(`{"name":"b"}`), "application/json")
		if resp.StatusCode != 401 && resp.StatusCode != 403 {
			t.Errorf("bearer %.8s made a token: %d %s", bearer, resp.StatusCode, body)
		}
	}
	// A personal token is no use on the ordinary API either.
	resp, _ := m.h().do("GET", "/v1/runs", tok, nil, "")
	if resp.StatusCode != 401 {
		t.Errorf("a personal token worked on /v1/runs: %d", resp.StatusCode)
	}
}

func (m *mcpHarness) h() *harness { return m.harness }

func TestMCPAcceptsOnlyPersonalTokensAndChecksTheRequest(t *testing.T) {
	m := newMCPHarness(t)
	tok := m.newToken(m.dev, map[string]any{"name": "a"})
	ping := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if resp, _ := m.mcpDo(tok, ping, nil); resp.StatusCode != 200 {
		t.Fatalf("a good token: %d", resp.StatusCode)
	}
	// Not a person's token: nothing, the shared server token, a made-up token, a session cookie.
	for name, token := range map[string]string{"none": "", "server token": testToken, "made up": apiTokenPrefix + strings.Repeat("a", 43), "wrong prefix": strings.TrimPrefix(tok, apiTokenPrefix)} {
		if resp, _ := m.mcpDo(token, ping, nil); resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if resp, _ := m.dev.do("POST", "/mcp", ping); resp.StatusCode != 401 {
		t.Errorf("a browser session was accepted: %d", resp.StatusCode)
	}
	// An expired token, and a token whose person is disabled.
	var tokens struct{ Tokens []APIToken }
	json.Unmarshal(m.dev.expect(200, "GET", "/v1/tokens", nil), &tokens)
	old := tokens.Tokens[0]
	old.ExpiresAt = time.Now().Add(-time.Minute)
	m.srv.docs.Put(context.Background(), kindAPIToken, old.ID, old)
	if resp, _ := m.mcpDo(tok, ping, nil); resp.StatusCode != 401 {
		t.Errorf("an expired token: %d", resp.StatusCode)
	}
	old.ExpiresAt = time.Now().Add(time.Hour)
	m.srv.docs.Put(context.Background(), kindAPIToken, old.ID, old)
	if resp, _ := m.mcpDo(tok, ping, nil); resp.StatusCode != 200 {
		t.Fatalf("restored: %d", resp.StatusCode)
	}
	m.admin.expect(200, "PATCH", "/v1/users/dev", map[string]any{"disabled": true})
	if resp, _ := m.mcpDo(tok, ping, nil); resp.StatusCode != 401 {
		t.Errorf("a disabled person's token: %d", resp.StatusCode)
	}
	m.admin.expect(200, "PATCH", "/v1/users/dev", map[string]any{"disabled": false})
	tok = m.newToken(m.login("dev"), map[string]any{"name": "again"}) // the old ones are gone for good

	// The request itself.
	if resp, _ := m.mcpDo(tok, ping, map[string]string{"Origin": "https://evil.example"}); resp.StatusCode != 403 {
		t.Errorf("a foreign origin: %d", resp.StatusCode)
	}
	if resp, _ := m.mcpDo(tok, ping, map[string]string{"Content-Type": "text/plain"}); resp.StatusCode != 415 {
		t.Errorf("text/plain: %d", resp.StatusCode)
	}
	if resp, _ := m.mcpDo(tok, []byte(strings.Repeat("x", mcpMaxBody+10)), nil); resp.StatusCode != 413 {
		t.Errorf("a big body: %d", resp.StatusCode)
	}
	if resp, _ := m.mcpDo(tok, []byte(`not json`), nil); resp.StatusCode != 400 {
		t.Errorf("not json: %d", resp.StatusCode)
	}
	if _, out := m.mcpDo(tok, []byte(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`), nil); !strings.Contains(string(out), "one message per request") {
		t.Errorf("a batch: %s", out)
	}
	if resp, out := m.mcpDo(tok, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), nil); resp.StatusCode != 202 || len(out) != 0 {
		t.Errorf("a notification: %d %q", resp.StatusCode, out)
	}
	if _, out := m.mcpDo(tok, []byte(`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`), nil); !strings.Contains(string(out), "-32601") {
		t.Errorf("unknown method: %s", out)
	}
	if resp, _ := m.dev.do("GET", "/mcp", nil); resp.StatusCode != 405 || resp.Header.Get("Allow") != "POST" {
		t.Errorf("GET: %d", resp.StatusCode)
	}
}

func TestMCPInitializeAndTheToolsEachRoleSees(t *testing.T) {
	m := newMCPHarness(t)
	m.turnOn()
	view := m.newToken(m.v, map[string]any{"name": "v"})
	dev := m.newToken(m.dev, map[string]any{"name": "d"})
	adm := m.newToken(m.admin, map[string]any{"name": "a"})

	var init struct {
		ProtocolVersion string
		ServerInfo      struct{ Name string }
		Capabilities    map[string]any
	}
	json.Unmarshal(m.rpc(view, "initialize", map[string]any{"protocolVersion": "2025-03-26"}).Result, &init)
	if init.ProtocolVersion != "2025-03-26" || init.ServerInfo.Name != "forgeline" || init.Capabilities["tools"] == nil {
		t.Errorf("initialize = %+v", init)
	}
	json.Unmarshal(m.rpc(view, "initialize", map[string]any{"protocolVersion": "1999-01-01"}).Result, &init)
	if init.ProtocolVersion != mcpDefaultProto {
		t.Errorf("an unknown version should get ours, got %s", init.ProtocolVersion)
	}

	v, d, a := m.toolNames(view), m.toolNames(dev), m.toolNames(adm)
	for _, n := range []string{"list_runs", "get_run", "get_step_log", "run_failure", "list_pipelines", "guard_findings", "k8s_clusters", "aws_accounts"} {
		if !v[n] {
			t.Errorf("a viewer lacks %s", n)
		}
	}
	for _, n := range []string{"propose_cloud_change", "propose_aws_change", "request_environment", "search_audit"} {
		if v[n] {
			t.Errorf("a viewer has %s", n)
		}
	}
	if !d["environment_templates"] || !d["request_environment"] || !d["propose_cloud_change"] || !d["propose_aws_change"] || !d["list_agents"] || d["search_audit"] {
		t.Errorf("developer tools: %v", d)
	}
	if !a["search_audit"] {
		t.Errorf("admin tools: %v", a)
	}
	// What is never exposed, whatever the role.
	for _, n := range []string{"call_aws", "aws_script", "ask_agent", "aws_search_documentation", "aws_read_documentation"} {
		if a[n] {
			t.Errorf("%s is exposed over MCP", n)
		}
	}
	for n := range a {
		if strings.HasPrefix(n, pluginToolPrefix) {
			t.Errorf("a plugin tool is exposed: %s", n)
		}
	}
}

func TestMCPToolsRunWithTheLowerOfTheTokenAndThePersonsRole(t *testing.T) {
	m := newMCPHarness(t)
	id := m.failedRun(twoStepYAML, 127, "", "sh: gotestsum: not found\n")
	adminTok := m.newToken(m.admin, map[string]any{"name": "full"})
	viewTok := m.newToken(m.admin, map[string]any{"name": "narrow", "role": "viewer"})

	if out, isErr := m.tool(adminTok, "search_audit", map[string]any{"limit": 5}); isErr {
		t.Errorf("an admin token can read the audit log: %s", out)
	}
	if out, isErr := m.tool(viewTok, "search_audit", nil); !isErr || !strings.Contains(out, "not available") {
		t.Errorf("a viewer-role token read the audit log: %q", out)
	}
	if out, isErr := m.tool(viewTok, "list_runs", map[string]any{"status": "failed"}); isErr || !strings.Contains(out, id) {
		t.Errorf("list_runs = %q (%v)", out, isErr)
	}
	out, isErr := m.tool(viewTok, "run_failure", map[string]any{"id": id})
	if isErr || !strings.Contains(out, "Step build failed") || !strings.Contains(out, "could not find the command") {
		t.Errorf("run_failure = %q", out)
	}
	if out, isErr := m.tool(viewTok, "run_failure", map[string]any{"id": "ghost"}); !isErr || !strings.Contains(out, "no run") {
		t.Errorf("run_failure on a missing run: %q", out)
	}
	if _, isErr := m.tool(viewTok, "call_aws", map[string]any{"cli_command": "aws s3 ls"}); !isErr {
		t.Error("call_aws ran over MCP")
	}
	// Even an administrator's token cannot reach what is not exposed over MCP.
	for _, n := range []string{"call_aws", "aws_script", "ask_agent", "aws_search_documentation", "plugin_x_y_z"} {
		if out, isErr := m.tool(adminTok, n, map[string]any{"cli_command": "aws s3 ls", "code": "x", "agent": "a", "task": "t"}); !isErr || !strings.Contains(out, "not available to this token") {
			t.Errorf("%s over MCP: %q (%v)", n, out, isErr)
		}
	}
	if _, isErr := m.tool(viewTok, "no_such_tool", nil); !isErr {
		t.Error("an unknown tool ran")
	}

	// Demoting the person demotes their token at once, with no new token needed.
	graceTok := m.newToken(m.admin2, map[string]any{"name": "g"})
	if out, isErr := m.tool(graceTok, "search_audit", nil); isErr {
		t.Fatalf("grace's admin token should work before the demotion: %s", out)
	}
	m.admin.expect(200, "PATCH", "/v1/users/grace", map[string]any{"role": "viewer"})
	if out, isErr := m.tool(graceTok, "search_audit", nil); !isErr {
		t.Errorf("a demoted person's token still read the audit log: %q", out)
	}
}

func TestMCPOutputNeverCarriesCredentialsAndEveryCallIsAudited(t *testing.T) {
	m := newMCPHarness(t)
	ctx := context.Background()
	run, _ := m.srv.cfg.Store.CreateRun(ctx, store.Run{PipelineName: "p", PipelineYAML: "x", Trigger: store.TriggerAPI}, []string{"s"})
	m.srv.cfg.Store.AppendLog(ctx, run.ID, "s", []byte("deploying with AKIAABCDEFGHIJKLMNOP and ghp_"+strings.Repeat("a", 36)+"\n-----BEGIN RSA PRIVATE KEY-----\nMIIsecretsecret\n-----END RSA PRIVATE KEY-----\ndone\n"))
	tok := m.newToken(m.v, map[string]any{"name": "v"})
	out, isErr := m.tool(tok, "get_step_log", map[string]any{"run_id": run.ID, "step_id": "s"})
	if isErr || !strings.Contains(out, credentialMask) || !strings.Contains(out, "done") {
		t.Fatalf("log = %q", out)
	}
	for _, leak := range []string{"AKIAABCDEFGHIJKLMNOP", "ghp_" + strings.Repeat("a", 36), "MIIsecretsecret", "BEGIN RSA PRIVATE KEY"} {
		if strings.Contains(out, leak) {
			t.Errorf("the output carries %q", leak)
		}
	}
	// An argument that is a credential is masked in the audit entry too.
	if out, isErr := m.tool(tok, "run_failure", map[string]any{"id": "AKIAABCDEFGHIJKLMNOP"}); !isErr || strings.Contains(out, "AKIAABCDEFGHIJKLMNOP") || !strings.Contains(out, credentialMask) {
		t.Errorf("an error message repeated a credential: %q", out)
	}
	m.tool(tok, "search_audit", nil) // refused for a viewer
	a := m.auditText()
	if strings.Count(a, `"action":"mcp.call"`) != 2 || !strings.Contains(a, `"action":"mcp.denied"`) || !strings.Contains(a, `"actor":"view"`) || !strings.Contains(a, "[token v] run_failure") || strings.Contains(a, "AKIAABCDEFGHIJKLMNOP") {
		t.Errorf("audit: %s", a)
	}
	// Bad tokens are audited, but one entry per address a minute.
	for i := 0; i < 5; i++ {
		m.mcpDo("flt_nope", []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), nil)
	}
	if n := strings.Count(m.auditText(), "missing, unknown, expired or unusable token"); n != 1 {
		t.Errorf("%d audit entries for a stream of bad tokens, want 1", n)
	}
}

func TestMCPOutputIsCutAtTheLookupLimit(t *testing.T) {
	m := newMCPHarness(t)
	yaml := "# " + strings.Repeat("padding ", 4000) + "\nversion: v1alpha1\nname: wide\nsteps:\n  - id: a\n    image: alpine\n    run: echo hi\n"
	m.dev.expect(201, "POST", "/v1/pipelines", map[string]any{"yaml": yaml})
	tok := m.newToken(m.v, map[string]any{"name": "v"})
	out, isErr := m.tool(tok, "get_pipeline", map[string]any{"id": "wide"})
	if isErr || len(out) < 1000 || len([]rune(out)) > maxLookupResult {
		t.Errorf("%d bytes, error %v", len(out), isErr)
	}
}

func TestMCPProposalsAreFiledAsTheTokensPersonAndRunNothing(t *testing.T) {
	m := newMCPHarness(t)
	m.turnOn()
	dev := m.newToken(m.dev, map[string]any{"name": "d"})
	view := m.newToken(m.v, map[string]any{"name": "v"})
	args := map[string]any{"kind": "instance-action", "instance_id": "i-0123456789abcdef0", "action": "stop", "reason": "It is idle over the weekend"}

	if out, isErr := m.tool(view, toolProposeCloud, args); !isErr || !strings.Contains(out, "not available") {
		t.Errorf("a viewer proposed: %q", out)
	}
	out, isErr := m.tool(dev, toolProposeCloud, args)
	if isErr || !strings.Contains(out, "Nothing has run") || !strings.Contains(out, "different administrator") {
		t.Fatalf("propose = %q", out)
	}
	var list struct{ Changes []CloudChange }
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/cloud/changes", nil), &list)
	if len(list.Changes) != 1 || list.Changes[0].ProposedBy != "dev" || list.Changes[0].Kind != kindInstanceAction {
		t.Fatalf("changes = %+v", list.Changes)
	}
	m.waitFor("the proposal to settle", func() bool {
		json.Unmarshal(m.admin.expect(200, "GET", "/v1/cloud/changes", nil), &list)
		return list.Changes[0].Status != stPreparing
	})
	if s := list.Changes[0].Status; s == stExecuting || s == stSucceeded {
		t.Errorf("a proposal made over MCP ran: %s", s)
	}
	for name, bad := range map[string]map[string]any{
		"another kind":    {"kind": "k8s-scale", "reason": "scale it up please"},
		"unknown field":   {"kind": "stack-delete", "stack": "demo", "reason": "remove the demo", "force": true},
		"short reason":    {"kind": "stack-delete", "stack": "demo", "reason": "no"},
		"protected stack": {"kind": "stack-delete", "stack": "CDKToolkit", "reason": "clean up the account"},
	} {
		if out, isErr := m.tool(dev, toolProposeCloud, bad); !isErr {
			t.Errorf("%s was accepted: %q", name, out)
		}
	}
	if !strings.Contains(m.auditText(), `"action":"cloud.propose"`) {
		t.Error("the proposal is not in the audit log")
	}
}

func TestMCPCallsAreRateLimitedPerToken(t *testing.T) {
	m := newMCPHarness(t)
	a, b := m.newToken(m.v, map[string]any{"name": "a"}), m.newToken(m.v, map[string]any{"name": "b"})
	for i := 0; i < mcpCallsPerMin; i++ {
		if out, isErr := m.tool(a, "list_pipelines", nil); isErr {
			t.Fatalf("call %d: %s", i, out)
		}
	}
	if out, isErr := m.tool(a, "list_pipelines", nil); !isErr || !strings.Contains(out, "Too many calls") {
		t.Errorf("past the limit: %q (%v)", out, isErr)
	}
	if out, isErr := m.tool(b, "list_pipelines", nil); isErr {
		t.Errorf("another token was limited too: %s", out)
	}
}

func TestMCPAdmitLimitsConcurrencyAndResetsEachMinute(t *testing.T) {
	var st mcpState
	now := time.Now()
	var dones []func()
	for i := 0; i < mcpMaxInflight; i++ {
		d, ok := st.admit("t", now)
		if !ok {
			t.Fatalf("call %d refused", i)
		}
		dones = append(dones, d)
	}
	if _, ok := st.admit("t", now); ok {
		t.Error("more than the allowed calls at once")
	}
	dones[0]()
	d, ok := st.admit("t", now)
	if !ok {
		t.Fatal("a finished call should free a slot")
	}
	d()
	for _, f := range dones[1:] {
		f()
	}
	if _, ok := st.admit("t", now.Add(2*time.Minute)); !ok {
		t.Error("the count should reset after a minute")
	}
	var full mcpState
	for i := 0; i < mcpCallsPerMin; i++ {
		d, ok := full.admit("u", now)
		if !ok {
			t.Fatalf("call %d refused", i)
		}
		d()
	}
	if _, ok := full.admit("u", now.Add(30*time.Second)); ok {
		t.Error("a 61st call inside the minute was allowed")
	}
	if _, ok := full.admit("u", now.Add(61*time.Second)); !ok {
		t.Error("the minute's count did not reset")
	}
}

func TestMaskCredentialsCoversEachFormat(t *testing.T) {
	for _, c := range []string{
		"AKIAABCDEFGHIJKLMNOP", "ASIAABCDEFGHIJKLMNOP", "ghp_" + strings.Repeat("b", 36), "github_pat_" + strings.Repeat("c", 60),
		"sk-ant-" + strings.Repeat("d", 30), "xoxb-1234567890-abcdef", "sk_live_" + strings.Repeat("e", 20),
		"-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc",
	} {
		got := maskCredentials("before " + c + " after")
		if strings.Contains(got, c[:12]) || !strings.Contains(got, credentialMask) || !strings.HasPrefix(got, "before ") {
			t.Errorf("%q -> %q", c, got)
		}
	}
	if got := maskCredentials("an ordinary line with nothing in it"); got != "an ordinary line with nothing in it" {
		t.Errorf("clean text changed: %q", got)
	}
}

func TestDeletingAPersonRemovesTheirTokens(t *testing.T) {
	m := newMCPHarness(t)
	tok := m.newToken(m.dev, map[string]any{"name": "d"})
	m.admin.expect(204, "DELETE", "/v1/users/dev", nil)
	if resp, _ := m.mcpDo(tok, []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), nil); resp.StatusCode != 401 {
		t.Errorf("a deleted person's token: %d", resp.StatusCode)
	}
	if docs, _ := m.srv.docs.List(context.Background(), kindAPIToken, docstoreAll); len(docs) != 0 {
		t.Errorf("%d tokens left behind", len(docs))
	}
}

var docstoreAll = docstore.ListOptions{}

func TestARevokedTokenIsNotWrittenBackByALateLastUsedUpdate(t *testing.T) {
	m := newMCPHarness(t)
	tok := m.newToken(m.dev, map[string]any{"name": "d"})
	var list struct{ Tokens []APIToken }
	json.Unmarshal(m.dev.expect(200, "GET", "/v1/tokens", nil), &list)
	id := list.Tokens[0].ID
	m.dev.expect(204, "DELETE", "/v1/tokens/"+id, nil)
	m.srv.touchToken(context.Background(), id, time.Now())
	var back APIToken
	if err := m.srv.docs.Get(context.Background(), kindAPIToken, id, &back); err == nil {
		t.Errorf("a late last-used update wrote the token back: %+v", back)
	}
	if resp, _ := m.mcpDo(tok, []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), nil); resp.StatusCode != 401 {
		t.Errorf("a revoked token came back: %d", resp.StatusCode)
	}
}

func TestDisablingAPersonRemovesTheirTokensSoEnablingDoesNotRevive(t *testing.T) {
	m := newMCPHarness(t)
	tok := m.newToken(m.dev, map[string]any{"name": "d"})
	m.admin.expect(200, "PATCH", "/v1/users/dev", map[string]any{"disabled": true})
	m.admin.expect(200, "PATCH", "/v1/users/dev", map[string]any{"disabled": false})
	if resp, _ := m.mcpDo(tok, []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), nil); resp.StatusCode != 401 {
		t.Errorf("an old token came back with the person: %d", resp.StatusCode)
	}
}

func TestExpiredTokensDoNotCountAgainstTheLimitAndAreRemoved(t *testing.T) {
	m := newMCPHarness(t)
	for i := 0; i < maxTokensPerUser; i++ {
		m.newToken(m.dev, map[string]any{"name": "t"})
	}
	m.dev.expect(409, "POST", "/v1/tokens", map[string]any{"name": "x"})
	var list struct{ Tokens []APIToken }
	json.Unmarshal(m.dev.expect(200, "GET", "/v1/tokens", nil), &list)
	for _, tk := range list.Tokens {
		tk.ExpiresAt = time.Now().Add(-time.Hour)
		m.srv.docs.Put(context.Background(), kindAPIToken, tk.ID, tk)
	}
	m.newToken(m.dev, map[string]any{"name": "fresh"})
	json.Unmarshal(m.dev.expect(200, "GET", "/v1/tokens", nil), &list)
	if len(list.Tokens) != 1 || list.Tokens[0].Name != "fresh" {
		t.Errorf("tokens = %+v", list.Tokens)
	}
}

func TestMCPOutputIsMaskedBeforeItIsCut(t *testing.T) {
	key := "ghp_" + strings.Repeat("k", 36)
	text := strings.Repeat("x", maxLookupResult-10) + " " + key + " tail"
	out := mcpOutput(text)
	if strings.Contains(out, "ghp_") || len([]rune(out)) > maxLookupResult {
		t.Errorf("a credential straddling the cut got through: …%q (%d runes)", out[len(out)-30:], len([]rune(out)))
	}
}

func TestMCPLimiterForgetsOnlyIdleOldEntriesWhenItIsFull(t *testing.T) {
	var st mcpState
	now := time.Now()
	st.window = map[string]*mcpBucket{"hot": {start: now, calls: mcpCallsPerMin}}
	for i := 0; i <= mcpMaxTracked; i++ {
		st.window[string(rune('a'+i%26))+strings.Repeat("z", i)] = &mcpBucket{start: now.Add(-5 * time.Minute)}
	}
	if _, ok := st.admit("someone-new", now); !ok {
		t.Fatal("a new token was refused")
	}
	if len(st.window) > 10 {
		t.Errorf("%d entries kept; the idle old ones should go", len(st.window))
	}
	if _, ok := st.admit("hot", now); ok {
		t.Error("clearing the map reset a token that was at its limit")
	}
	st.deniedAt = map[string]time.Time{"1.1.1.1": now}
	for i := 0; i <= mcpMaxTracked; i++ {
		st.deniedAt["old"+strings.Repeat("z", i)] = now.Add(-time.Hour)
	}
	if st.shouldAuditDenied("1.1.1.1", now) {
		t.Error("an address in its quiet minute was audited again after the map was trimmed")
	}
}

func TestRunFailureNeverReturnsACredentialToTheAssistantEither(t *testing.T) {
	m := newMCPHarness(t)
	id := m.failedRun(twoStepYAML, 1, "", "using AKIAABCDEFGHIJKLMNOP failed\n")
	out, err := m.srv.lookupRunFailure(context.Background(), json.RawMessage(`{"id":"`+id+`"}`))
	if err != nil || strings.Contains(out, "AKIAABCDEFGHIJKLMNOP") || !strings.Contains(out, credentialMask) {
		t.Errorf("%q (%v)", out, err)
	}
}

func TestProposalsFiledOverMCPRecordTheClientAddress(t *testing.T) {
	m := newMCPHarness(t)
	m.turnOn()
	dev := m.newToken(m.dev, map[string]any{"name": "d"})
	out, isErr := m.tool(dev, toolProposeAWS, map[string]any{"title": "Scale web to two tasks", "reason": "web is over-provisioned at three tasks", "code": changeScript})
	if isErr || !strings.Contains(out, "Nothing has run") {
		t.Fatalf("propose = %q", out)
	}
	var found bool
	var events struct{ Events []AuditEvent }
	raw := m.admin.expect(200, "GET", "/v1/audit?limit=100", nil)
	json.Unmarshal(raw, &events)
	for _, e := range events.Events {
		if e.Action == "ai.proposal.create" && e.IP != "" {
			found = true
		}
	}
	if !found {
		t.Errorf("no ai.proposal.create entry with an address: %s", raw)
	}
}

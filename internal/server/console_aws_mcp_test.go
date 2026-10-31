package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forgelinehq/forgeline/internal/authn"
	"github.com/forgelinehq/forgeline/internal/mcp"
)

// fakeMCP stands in for the AWS MCP Server: it records calls and, for run_script, reports the
// calls the script declares (or extra ones, when sneak is set), like the real sandbox does.
type fakeMCP struct {
	mu      sync.Mutex
	calls   []string // tool names
	args    []map[string]any
	tools   []string
	sneak   string // "service Operation" to report in addition
	started int
	starts  []awsSettings // the connection each proxy was started for
	runs    []awsSettings // the connection each run_script call went through
	slow    chan struct{} // when set, run_script waits for it
	failErr string
}

func newFakeMCP() *fakeMCP {
	return &fakeMCP{tools: []string{"aws___get_tasks", "aws___list_regions", "aws___run_script", "aws___search_documentation", "aws___read_documentation"}}
}

func (f *fakeMCP) count(tool string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == tool {
			n++
		}
	}
	return n
}

func (f *fakeMCP) start(_ context.Context, st awsSettings) (mcpConn, error) {
	f.mu.Lock()
	f.started++
	f.starts = append(f.starts, st)
	f.mu.Unlock()
	return &fakeMCPConn{f, st}, nil
}

type fakeMCPConn struct {
	f  *fakeMCP
	st awsSettings
}

func (c *fakeMCPConn) Close() {}
func (c *fakeMCPConn) Tools(context.Context) ([]mcp.Tool, error) {
	var ts []mcp.Tool
	for _, n := range c.f.tools {
		ts = append(ts, mcp.Tool{Name: n})
	}
	return ts, nil
}
func (c *fakeMCPConn) Call(_ context.Context, name string, args map[string]any) (mcp.Result, error) {
	f := c.f
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.args = append(f.args, args)
	if name == "aws___run_script" {
		f.runs = append(f.runs, c.st)
	}
	slow, sneak, fail := f.slow, f.sneak, f.failErr
	f.mu.Unlock()
	if fail != "" {
		return mcp.Result{}, errors.New(fail)
	}
	if name != "aws___run_script" {
		return mcp.Result{Text: "docs for " + name}, nil
	}
	if slow != nil {
		<-slow
	}
	code, _ := args["code"].(string)
	declared, _ := checkScript(code, true)
	if declared == nil {
		declared, _ = checkScript(code, false)
	}
	var api []map[string]string
	for _, d := range declared {
		api = append(api, map[string]string{"service": d.Service, "operation": d.Operation, "status": "success"})
	}
	if sneak != "" {
		p := strings.SplitN(sneak, " ", 2)
		api = append(api, map[string]string{"service": p[0], "operation": p[1], "status": "success"})
	}
	st, _ := json.Marshal(map[string]any{"status": "success", "return_value": "ok", "api_calls": api})
	return mcp.Result{Text: string(st), Structured: st}, nil
}

type mcpHarness struct {
	*harness
	llm                   *scriptedLLM
	mcp                   *fakeMCP
	admin, admin2, dev, v *browser
}

func newMCPHarness(t *testing.T) *mcpHarness {
	t.Helper()
	llm := newScriptedLLM(t, "done")
	h, admin, dev, _ := assistantHarness(t, llm, "x")
	admin.createUser("grace", authn.Admin)
	admin.createUser("view", authn.Viewer)
	m := &mcpHarness{harness: h, llm: llm, mcp: newFakeMCP(), admin: admin, admin2: h.login("grace"), dev: dev, v: h.login("view")}
	h.srv.mcpStart = m.mcp.start
	h.srv.run = func(_ context.Context, name string, args ...string) ([]byte, string, error) {
		return []byte(`{"Account":"111122223333","Arn":"arn:aws:sts::111122223333:assumed-role/Admin/ada","UserId":"AROA:ada"}`), "", nil
	}
	return m
}

func (m *mcpHarness) turnOn() {
	m.t.Helper()
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})
	m.admin.expect(200, "PUT", "/v1/integrations/aws/mcp", map[string]any{"enabled": true})
}

// ask has the scripted model reply with the given lookup, then "done", and returns the chat reply.
func (m *mcpHarness) ask(c *browser, lookup string) ChatReply {
	m.llm.mu.Lock()
	m.llm.replies, m.llm.reqs = []string{lookup, "done"}, nil
	m.llm.mu.Unlock()
	var r ChatReply
	json.Unmarshal(c.expect(200, "POST", "/v1/assistant/chat", map[string]any{"message": "go"}), &r)
	return r
}

const (
	readScript   = `r = await call_boto3(service_name="ecs", operation_name="ListClusters")` + "\nresult = r\nresult"
	changeScript = `r = await call_boto3(service_name="ecs", operation_name="UpdateService", params={"cluster": "payments", "service": "web", "desiredCount": 2})` + "\nresult = r\nresult"
)

func (m *mcpHarness) waitProposal(id string, notIn ...string) AIProposal {
	m.t.Helper()
	var p AIProposal
	m.waitFor("proposal "+id+" to settle", func() bool {
		json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/proposals/"+id, nil), &p)
		for _, s := range notIn {
			if p.Status == s {
				return false
			}
		}
		return true
	})
	return p
}

func TestAWSMCPSettingsNeedAWSAndTheScriptTool(t *testing.T) {
	m := newMCPHarness(t)
	m.dev.expect(403, "PUT", "/v1/integrations/aws/mcp", map[string]any{"enabled": true})
	// AWS must be connected first.
	if b := m.admin.expect(422, "PUT", "/v1/integrations/aws/mcp", map[string]any{"enabled": true}); !strings.Contains(string(b), "connect an AWS account") {
		t.Errorf("body = %s", b)
	}
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})
	// A server without the script tool is refused and nothing is saved as enabled.
	m.mcp.tools = []string{"aws___list_regions"}
	if b := m.admin.expect(422, "PUT", "/v1/integrations/aws/mcp", map[string]any{"enabled": true}); !strings.Contains(string(b), "aws___run_script") {
		t.Errorf("body = %s", b)
	}
	var v AWSMCPJSON
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/integrations/aws/mcp", nil), &v)
	if v.Enabled {
		t.Error("enabled after a refusal")
	}
	m.mcp.tools = newFakeMCP().tools
	json.Unmarshal(m.admin.expect(200, "PUT", "/v1/integrations/aws/mcp", map[string]any{"enabled": true}), &v)
	if !v.Enabled || !contains(v.Tools, "aws___run_script") {
		t.Errorf("view = %+v", v)
	}
	// Off again: the tools disappear from the assistant's prompt and calls are refused.
	m.admin.expect(200, "PUT", "/v1/integrations/aws/mcp", map[string]any{"enabled": false})
	m.ask(m.admin, lookupReply("aws_script", map[string]string{"code": readScript}))
	if !strings.Contains(m.llm.lastUser(1), "not turned on") || m.mcp.count("aws___run_script") != 0 {
		t.Errorf("a disabled server ran a script: %s", m.llm.lastUser(1))
	}
}

func TestAssistantReadScriptsRunThroughMCPOnlyWhenTheyAreReads(t *testing.T) {
	m := newMCPHarness(t)
	m.turnOn()

	r := m.ask(m.admin, lookupReply("aws_script", map[string]string{"code": readScript}))
	if len(r.Lookups) != 1 || !strings.Contains(m.llm.lastUser(1), `"status":"success"`) || m.mcp.count("aws___run_script") != 1 {
		t.Fatalf("read script: %+v / %s", r, m.llm.lastUser(1))
	}
	// With the MCP server on there is one path to AWS: the CLI tool is off.
	m.ask(m.admin, lookupReply("call_aws", map[string]string{"cli_command": "aws ecs list-clusters"}))
	if !strings.Contains(m.llm.lastUser(1), "use aws_script") {
		t.Errorf("call_aws still works beside the MCP server: %s", m.llm.lastUser(1))
	}
	// A script that changes something never reaches the server, and the model is told why.
	m.ask(m.admin, lookupReply("aws_script", map[string]string{"code": changeScript}))
	if !strings.Contains(m.llm.lastUser(1), "not a read operation") || m.mcp.count("aws___run_script") != 1 {
		t.Errorf("a change ran as a read: %s", m.llm.lastUser(1))
	}
	// So does one that hides its calls.
	m.ask(m.admin, lookupReply("aws_script", map[string]string{"code": "op = 'Update' + 'Service'\nawait call_boto3(service_name='ecs', operation_name=op)"}))
	if m.mcp.count("aws___run_script") != 1 {
		t.Error("a script with a computed operation reached the server")
	}
	// A developer cannot run scripts and is not offered the tool.
	m.ask(m.dev, lookupReply("aws_script", map[string]string{"code": readScript}))
	if !strings.Contains(m.llm.lastUser(1), "not available to your role") || m.mcp.count("aws___run_script") != 1 {
		t.Errorf("a developer ran a script: %s", m.llm.lastUser(1))
	}
	m.llm.mu.Lock()
	sys := m.llm.reqs[0]["system"].(string)
	m.llm.mu.Unlock()
	if strings.Contains(sys, "aws_script") || !strings.Contains(sys, "propose_aws_change") {
		t.Errorf("developer prompt lists the wrong tools")
	}

	// If the sandbox reports a call the script did not declare, the result is thrown away.
	m.mcp.mu.Lock()
	m.mcp.sneak = "ec2 TerminateInstances"
	m.mcp.mu.Unlock()
	m.ask(m.admin, lookupReply("aws_script", map[string]string{"code": readScript}))
	got := m.llm.lastUser(1)
	if !strings.Contains(got, "did not declare") || strings.Contains(got, `"status":"success"`) {
		t.Errorf("an undeclared call was not caught: %s", got)
	}
	if !strings.Contains(string(m.admin.expect(200, "GET", "/v1/audit", nil)), "aws_script") {
		t.Error("script lookups are not audited")
	}
}

func TestAssistantDocumentationToolsForwardOnlyTheirOwnArguments(t *testing.T) {
	m := newMCPHarness(t)
	m.turnOn()
	m.ask(m.dev, lookupReply("aws_search_documentation", map[string]any{"search_phrase": "ecs canary", "limit": 3, "code": "evil", "cli_command": "x"}))
	m.mcp.mu.Lock()
	defer m.mcp.mu.Unlock()
	if len(m.mcp.args) == 0 {
		t.Fatal("no call")
	}
	a := m.mcp.args[len(m.mcp.args)-1]
	if a["search_phrase"] != "ecs canary" || a["code"] != nil || a["cli_command"] != nil || len(a) != 2 {
		t.Errorf("forwarded %v", a)
	}
}

func (m *mcpHarness) propose(c *browser, code string) (ChatReply, string) {
	r := m.ask(c, lookupReply("propose_aws_change", map[string]string{"title": "Scale web to two tasks", "reason": "web is over-provisioned at three tasks", "code": code}))
	if len(r.Proposals) != 1 {
		return r, ""
	}
	return r, r.Proposals[0]
}

func TestProposedChangesNeedAnotherAdministratorAndRunExactlyWhatWasApproved(t *testing.T) {
	m := newMCPHarness(t)
	m.turnOn()

	// The assistant can only file; nothing reaches AWS.
	_, id := m.propose(m.dev, changeScript)
	if id == "" || m.mcp.count("aws___run_script") != 0 {
		t.Fatalf("id = %q, scripts run = %d", id, m.mcp.count("aws___run_script"))
	}
	var p AIProposal
	json.Unmarshal(m.v.expect(200, "GET", "/v1/ai/proposals/"+id, nil), &p)
	if p.Status != apProposed || p.ProposedBy != "dev" || !p.ViaAI || len(p.Calls) != 1 || p.Calls[0].Operation != "UpdateService" || p.Code != changeScript || p.Risk != "normal" {
		t.Fatalf("proposal = %+v", p)
	}

	// Who may decide: viewers and developers cannot; wrong text is refused; a changed account is refused.
	m.v.expect(403, "POST", "/v1/ai/proposals/"+id+"/approve", map[string]any{"hash": p.Hash})
	m.dev.expect(403, "POST", "/v1/ai/proposals/"+id+"/approve", map[string]any{"hash": p.Hash})
	m.admin2.expect(409, "POST", "/v1/ai/proposals/"+id+"/approve", map[string]any{"hash": "0000"})
	m.admin2.expect(409, "POST", "/v1/ai/proposals/"+id+"/approve", map[string]any{})
	if m.mcp.count("aws___run_script") != 0 {
		t.Fatal("a refused approval ran the script")
	}

	// An administrator who proposed it cannot approve it.
	_, own := m.propose(m.admin, changeScript)
	var ownP AIProposal
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/proposals/"+own, nil), &ownP)
	m.admin.expect(403, "POST", "/v1/ai/proposals/"+own+"/approve", map[string]any{"hash": ownP.Hash})

	// A different administrator approves; exactly the filed script runs.
	m.admin2.expect(202, "POST", "/v1/ai/proposals/"+id+"/approve", map[string]any{"hash": p.Hash, "note": "checked the service"})
	done := m.waitProposal(id, apApplying)
	if done.Status != apApplied || done.DecidedBy != "grace" || len(done.RanCalls) != 1 || done.RanCalls[0].Operation != "UpdateService" {
		t.Fatalf("applied = %+v", done)
	}
	m.mcp.mu.Lock()
	ranCode := m.mcp.args[len(m.mcp.args)-1]["code"]
	m.mcp.mu.Unlock()
	if ranCode != changeScript {
		t.Errorf("ran %q, approved %q", ranCode, changeScript)
	}
	// It cannot be approved twice.
	m.admin2.expect(409, "POST", "/v1/ai/proposals/"+id+"/approve", map[string]any{"hash": p.Hash})
	if m.mcp.count("aws___run_script") != 1 {
		t.Errorf("scripts run = %d", m.mcp.count("aws___run_script"))
	}
	a := string(m.admin.expect(200, "GET", "/v1/audit", nil))
	for _, want := range []string{"ai.proposal.create", "ai.proposal.approve", "ai.proposal.apply"} {
		if !strings.Contains(a, want) {
			t.Errorf("audit has no %s", want)
		}
	}
}

func TestProposalsRefuseWhatTheAssistantMayNotPropose(t *testing.T) {
	m := newMCPHarness(t)
	m.turnOn()
	for name, code := range map[string]string{
		"read only": readScript,
		"delete":    `await call_boto3(service_name="ecs", operation_name="DeleteService", params={})`,
		"iam":       `await call_boto3(service_name="iam", operation_name="AttachRolePolicy", params={})`,
		"computed":  "op = 'Update' + 'Service'\nawait call_boto3(service_name='ecs', operation_name=op)",
		"import os": "import os\n" + changeScript,
	} {
		if _, id := m.propose(m.dev, code); id != "" {
			t.Errorf("%s: a proposal was filed", name)
		}
	}
	// Short reasons are refused.
	r := m.ask(m.dev, lookupReply("propose_aws_change", map[string]string{"title": "x", "reason": "y", "code": changeScript}))
	if len(r.Proposals) != 0 {
		t.Error("a proposal without a real title and reason was filed")
	}
	var list struct{ Proposals []AIProposal }
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/proposals", nil), &list)
	if len(list.Proposals) != 0 {
		t.Errorf("proposals = %d", len(list.Proposals))
	}
}

func TestProposalLifecycleRejectCancelExpireAndConnectionChange(t *testing.T) {
	m := newMCPHarness(t)
	m.turnOn()
	hash := func(id string) string {
		var p AIProposal
		json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/proposals/"+id, nil), &p)
		return p.Hash
	}

	_, a := m.propose(m.dev, changeScript)
	m.dev.expect(403, "POST", "/v1/ai/proposals/"+a+"/reject", map[string]any{})
	m.admin2.expect(200, "POST", "/v1/ai/proposals/"+a+"/reject", map[string]any{})
	m.admin2.expect(409, "POST", "/v1/ai/proposals/"+a+"/approve", map[string]any{"hash": hash(a)})

	_, b := m.propose(m.dev, changeScript)
	m.v.expect(403, "POST", "/v1/ai/proposals/"+b+"/cancel", map[string]any{})
	m.dev.expect(200, "POST", "/v1/ai/proposals/"+b+"/cancel", map[string]any{})

	// Old proposals expire and cannot be approved.
	_, c := m.propose(m.dev, changeScript)
	var p AIProposal
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/proposals/"+c, nil), &p)
	p.ProposedAt = time.Now().Add(-25 * time.Hour)
	m.srv.docs.Put(context.Background(), kindAIProposal, c, p)
	if b := m.admin2.expect(409, "POST", "/v1/ai/proposals/"+c+"/approve", map[string]any{"hash": p.Hash}); !strings.Contains(string(b), "older than 24 hours") {
		t.Errorf("expiry body = %s", b)
	}

	// Changing the AWS connection voids waiting proposals.
	_, d := m.propose(m.dev, changeScript)
	h := hash(d)
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "eu-west-1"})
	if b := m.admin2.expect(409, "POST", "/v1/ai/proposals/"+d+"/approve", map[string]any{"hash": h}); !strings.Contains(string(b), "connection changed") {
		t.Errorf("connection body = %s", b)
	}
	if m.mcp.count("aws___run_script") != 0 {
		t.Error("a refused approval ran a script")
	}
}

func TestAnApprovedChangeThatDoesMoreThanApprovedIsReportedAsFailed(t *testing.T) {
	m := newMCPHarness(t)
	m.turnOn()
	_, id := m.propose(m.dev, changeScript)
	var p AIProposal
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/proposals/"+id, nil), &p)
	m.mcp.mu.Lock()
	m.mcp.sneak = "ec2 RunInstances"
	m.mcp.mu.Unlock()
	m.admin2.expect(202, "POST", "/v1/ai/proposals/"+id+"/approve", map[string]any{"hash": p.Hash})
	done := m.waitProposal(id, apApplying)
	if done.Status != apFailed || !strings.Contains(done.Result, "not in the approved script") || len(done.RanCalls) != 2 {
		t.Fatalf("result = %+v", done)
	}
}

func TestOnlyOneApprovedChangeIsAppliedAtATime(t *testing.T) {
	m := newMCPHarness(t)
	m.turnOn()
	m.mcp.slow = make(chan struct{})
	_, a := m.propose(m.dev, changeScript)
	_, b := m.propose(m.dev, changeScript)
	var pa, pb AIProposal
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/proposals/"+a, nil), &pa)
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/proposals/"+b, nil), &pb)
	m.admin2.expect(202, "POST", "/v1/ai/proposals/"+a+"/approve", map[string]any{"hash": pa.Hash})
	m.admin2.expect(409, "POST", "/v1/ai/proposals/"+b+"/approve", map[string]any{"hash": pb.Hash})
	close(m.mcp.slow)
	if got := m.waitProposal(a, apApplying); got.Status != apApplied {
		t.Fatalf("first = %+v", got)
	}
	m.waitFor("the lock to clear", func() bool {
		resp, _ := m.admin2.do("POST", "/v1/ai/proposals/"+b+"/approve", map[string]any{"hash": pb.Hash})
		return resp.StatusCode == 202
	})
	m.waitProposal(b, apApplying)
}

package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const rebootReq = "reboot"

func (m *mcpHarness) cloudChange(c *browser, want int, account string) CloudChange {
	m.t.Helper()
	body := map[string]any{"kind": "instance-action", "instance_id": "i-0123456789abcdef0", "action": rebootReq, "reason": "The web server is stuck"}
	if account != "" {
		body["account"] = account
	}
	var ch CloudChange
	json.Unmarshal(c.expect(want, "POST", "/v1/cloud/changes", body), &ch)
	return ch
}

func (m *mcpHarness) settledChange(id string, notIn ...string) CloudChange {
	m.t.Helper()
	var ch CloudChange
	m.waitFor("change "+id, func() bool {
		_, b := m.admin.do("GET", "/v1/cloud/changes/"+id, nil)
		json.Unmarshal(b, &ch)
		for _, s := range notIn {
			if ch.Status == s {
				return false
			}
		}
		return true
	})
	return ch
}

func (m *mcpHarness) allowWrites(id string, on bool) {
	m.t.Helper()
	m.admin.expect(200, "PATCH", "/v1/aws/accounts/"+id, map[string]any{"writes": on})
}

func TestAccountWritesAreOffByDefaultAndOnlyAnAdministratorSwitchesThemOn(t *testing.T) {
	m, st := accountHarness(t)
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	for _, a := range listAccounts(t, m.v) {
		if a.ID == primaryAccount && !a.Writes || a.ID == "staging" && a.Writes {
			t.Errorf("writes of %s = %v", a.ID, a.Writes)
		}
	}
	m.dev.expect(403, "PATCH", "/v1/aws/accounts/staging", map[string]any{"writes": true})
	m.v.expect(403, "PATCH", "/v1/aws/accounts/staging", map[string]any{"writes": true})
	m.admin.expect(400, "PATCH", "/v1/aws/accounts/staging", map[string]any{})
	m.admin.expect(400, "PATCH", "/v1/aws/accounts/primary", map[string]any{"writes": false})
	m.admin.expect(404, "PATCH", "/v1/aws/accounts/ghost", map[string]any{"writes": true})

	// A developer cannot propose a change for it, and nothing reaches the account.
	if b := m.cloudChangeRaw(m.dev, 403, "staging"); !strings.Contains(string(b), "has not allowed changes") {
		t.Errorf("body = %s", b)
	}
	m.cloudChange(m.dev, 404, "ghost")
	if st.called("staging", "ec2") != 0 {
		t.Error("a refused proposal reached the account")
	}
	m.allowWrites("staging", true)
	for _, a := range listAccounts(t, m.v) {
		if a.ID == "staging" && !a.Writes {
			t.Error("writes did not turn on")
		}
	}
	if log := string(m.admin.expect(200, "GET", "/v1/audit?limit=50", nil)); !strings.Contains(log, "integration.aws.account.writes") {
		t.Errorf("audit: %s", log)
	}
}

func (m *mcpHarness) cloudChangeRaw(c *browser, want int, account string) []byte {
	m.t.Helper()
	return c.expect(want, "POST", "/v1/cloud/changes", map[string]any{"kind": "instance-action", "instance_id": "i-0123456789abcdef0", "action": rebootReq,
		"reason": "The web server is stuck", "account": account})
}

func TestACloudChangeRunsInTheAccountItWasPreparedFor(t *testing.T) {
	m, st := accountHarness(t)
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	m.allowWrites("staging", true)

	ch := m.cloudChange(m.dev, 202, "staging")
	ch = m.settledChange(ch.ID, "preparing")
	if ch.Status != "proposed" || ch.Conn != "staging" || ch.ConnName != "Staging" || ch.Account != "444455556666" || ch.Profile != "staging" ||
		!strings.Contains(ch.Title, "in Staging") || len(ch.Preview) != 1 || !strings.Contains(ch.Preview[0].Resource, "web-444455556666") {
		t.Fatalf("change = %+v", ch)
	}
	m.admin.expect(202, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]any{})
	if done := m.settledChange(ch.ID, "proposed", "executing"); done.Status != "succeeded" {
		t.Fatalf("done = %+v", done)
	}
	if st.called("staging", "ec2 reboot-instances") != 1 || st.called("dev", "ec2 reboot-instances") != 0 {
		t.Errorf("calls: %v", st.calls)
	}
	// The primary connection still works as before and is a different target.
	p := m.settledChange(m.cloudChange(m.dev, 202, "").ID, "preparing")
	if p.Conn != "" || p.Profile != "dev" || p.target() == ch.target() || !strings.HasPrefix(ch.target(), "acct/staging/") {
		t.Errorf("primary = %+v (target %s vs %s)", p, p.target(), ch.target())
	}
}

func TestTurningWritesOffOrRemovingTheAccountCancelsWhatWasWaiting(t *testing.T) {
	m, st := accountHarness(t)
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	m.allowWrites("staging", true)
	a := m.settledChange(m.cloudChange(m.dev, 202, "staging").ID, "preparing")
	m.allowWrites("staging", false)
	if got := m.settledChange(a.ID); got.Status != "canceled" || !strings.Contains(got.DecisionNote, "turned off") {
		t.Fatalf("after switching off: %+v", got)
	}
	m.admin.expect(409, "POST", "/v1/cloud/changes/"+a.ID+"/approve", map[string]any{})

	m.allowWrites("staging", true)
	b := m.settledChange(m.cloudChange(m.dev, 202, "staging").ID, "preparing")
	m.admin.expect(204, "DELETE", "/v1/aws/accounts/staging", nil)
	if got := m.settledChange(b.ID); got.Status != "canceled" || !strings.Contains(got.DecisionNote, "removed") {
		t.Fatalf("after removal: %+v", got)
	}
	m.admin.expect(409, "POST", "/v1/cloud/changes/"+b.ID+"/approve", map[string]any{})
	if st.called("staging", "ec2 reboot-instances") != 0 {
		t.Error("a canceled change ran")
	}
	// The primary's waiting changes are untouched by any of it.
	p := m.settledChange(m.cloudChange(m.dev, 202, "").ID, "preparing")
	m.admin.expect(404, "PATCH", "/v1/aws/accounts/staging", map[string]any{"writes": false}) // gone; nothing else changes
	if got := m.settledChange(p.ID); got.Status != "proposed" {
		t.Errorf("primary change = %+v", got)
	}
}

func TestApprovalRefusesAnAccountThatNowPointsElsewhere(t *testing.T) {
	m, st := accountHarness(t)
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	m.allowWrites("staging", true)
	ch := m.settledChange(m.cloudChange(m.dev, 202, "staging").ID, "preparing")
	st.mu.Lock()
	st.accounts["staging"] = "999900001111" // the profile now signs in to another AWS account
	st.mu.Unlock()
	if b := m.admin.expect(409, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]any{}); !strings.Contains(string(b), "999900001111") {
		t.Errorf("body = %s", b)
	}
	// Proposing while it points elsewhere is refused too.
	if b := m.cloudChangeRaw(m.dev, 409, "staging"); !strings.Contains(string(b), "now points at AWS account") {
		t.Errorf("body = %s", b)
	}
	if st.called("staging", "ec2 reboot-instances") != 0 {
		t.Error("it ran")
	}
}

func (m *mcpHarness) mcpOn() {
	m.t.Helper()
	m.admin.expect(200, "PUT", "/v1/integrations/aws/mcp", map[string]any{"enabled": true})
}

func (m *mcpHarness) proposeFor(c *browser, account string) (ChatReply, string) {
	args := map[string]string{"title": "Scale web to two tasks", "reason": "web is over-provisioned at three tasks", "code": changeScript}
	if account != "" {
		args["account"] = account
	}
	r := m.ask(c, lookupReply("propose_aws_change", args))
	if len(r.Proposals) != 1 {
		return r, ""
	}
	return r, r.Proposals[0]
}

func TestAssistantProposalsTargetAChosenAccountThroughItsOwnProxy(t *testing.T) {
	m, _ := accountHarness(t)
	m.mcpOn()
	addAccount(m.admin, 201, "Staging", "staging", "staging")

	// Not allowed yet: nothing is filed, and the model is told why.
	r, id := m.proposeFor(m.dev, "staging")
	if told := m.llm.lastUser(1); id != "" || !strings.Contains(told, "has not allowed changes") {
		t.Fatalf("reply = %+v, the model was told %q", r, told)
	}
	if _, id := m.proposeFor(m.dev, "ghost"); id != "" {
		t.Fatal("a proposal for an unknown account was filed")
	}
	m.allowWrites("staging", true)
	_, id = m.proposeFor(m.dev, "staging")
	if id == "" {
		t.Fatal("no proposal filed")
	}
	var p AIProposal
	json.Unmarshal(m.v.expect(200, "GET", "/v1/ai/proposals/"+id, nil), &p)
	if p.Account != "staging" || p.AccountName != "Staging" || p.Target != "acct/staging/444455556666/staging/eu-west-1" {
		t.Fatalf("proposal = %+v", p)
	}
	// Approval runs it through a proxy for staging, not the primary.
	m.admin2.expect(202, "POST", "/v1/ai/proposals/"+id+"/approve", map[string]any{"hash": p.Hash})
	if done := m.waitProposal(id, apApplying); done.Status != apApplied {
		t.Fatalf("done = %+v", done)
	}
	m.mcp.mu.Lock()
	last := m.mcp.runs[len(m.mcp.runs)-1]
	m.mcp.mu.Unlock()
	if last.Profile != "staging" || last.Region != "eu-west-1" {
		t.Errorf("ran through %+v", last)
	}
	// The primary still goes through the primary's connection.
	_, pid := m.proposeFor(m.dev, "")
	var pp AIProposal
	json.Unmarshal(m.v.expect(200, "GET", "/v1/ai/proposals/"+pid, nil), &pp)
	if pp.Account != "" || pp.Target != "dev/ap-south-1" {
		t.Fatalf("primary proposal = %+v", pp)
	}
	m.admin2.expect(202, "POST", "/v1/ai/proposals/"+pid+"/approve", map[string]any{"hash": pp.Hash})
	m.waitProposal(pid, apApplying)
	m.mcp.mu.Lock()
	last = m.mcp.runs[len(m.mcp.runs)-1]
	m.mcp.mu.Unlock()
	if last.Profile != "dev" {
		t.Errorf("primary ran through %+v", last)
	}
}

func TestAssistantProposalsAreBoundToTheirAccount(t *testing.T) {
	m, st := accountHarness(t)
	m.mcpOn()
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	m.allowWrites("staging", true)

	// Switching writes off cancels the waiting proposal.
	_, a := m.proposeFor(m.dev, "staging")
	m.allowWrites("staging", false)
	if got := m.waitProposal(a); got.Status != apCancelled || !strings.Contains(got.Note, "turned off") {
		t.Fatalf("after switching off: %+v", got)
	}
	m.allowWrites("staging", true)

	// Re-pointing the stored proposal at another account breaks the target the approver signed.
	_, b := m.proposeFor(m.dev, "staging")
	var bp AIProposal
	json.Unmarshal(m.v.expect(200, "GET", "/v1/ai/proposals/"+b, nil), &bp)
	moved := bp
	moved.Account, moved.AccountName = "", ""
	if err := m.srv.docs.Put(context.Background(), kindAIProposal, b, moved); err != nil {
		t.Fatal(err)
	}
	m.admin2.expect(409, "POST", "/v1/ai/proposals/"+b+"/approve", map[string]any{"hash": bp.Hash})

	// The profile pointing at another AWS account is caught at approval.
	_, c := m.proposeFor(m.dev, "staging")
	var cp AIProposal
	json.Unmarshal(m.v.expect(200, "GET", "/v1/ai/proposals/"+c, nil), &cp)
	st.mu.Lock()
	st.accounts["staging"] = "999900001111"
	st.mu.Unlock()
	if body := m.admin2.expect(409, "POST", "/v1/ai/proposals/"+c+"/approve", map[string]any{"hash": cp.Hash}); !strings.Contains(string(body), "999900001111") {
		t.Errorf("body = %s", body)
	}
	st.mu.Lock()
	st.accounts["staging"] = "444455556666"
	st.mu.Unlock()

	// Writes switched off behind the proposal's back (not through the handler) stop it at apply time.
	var acct AWSAccount
	m.srv.docs.Get(context.Background(), kindAWSAccount, "staging", &acct)
	runsOf := func() int { m.mcp.mu.Lock(); defer m.mcp.mu.Unlock(); return len(m.mcp.runs) }
	runsBefore := runsOf()
	acct.Writes = false
	m.srv.docs.Put(context.Background(), kindAWSAccount, "staging", acct)
	cp.Status = apApplying
	m.srv.docs.Put(context.Background(), kindAIProposal, c, cp)
	m.srv.aiMu.Lock()
	m.srv.aiBusy = true
	m.srv.aiMu.Unlock()
	m.srv.applyProposal(cp, Principal{Name: "grace"})
	if got := m.waitProposal(c, apApplying); got.Status != apFailed || !strings.Contains(got.Result, "changed before it ran") || runsOf() != runsBefore {
		t.Errorf("apply = %+v, runs %d -> %d", got, runsBefore, runsOf())
	}
}

func TestReadScriptsCanTargetAnyAccountAndTheListSaysWhichTakeChanges(t *testing.T) {
	m, _ := accountHarness(t)
	m.mcpOn()
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	m.ask(m.admin, lookupReply("aws_script", map[string]string{"code": readScript, "account": "staging"}))
	m.mcp.mu.Lock()
	last := m.mcp.runs[len(m.mcp.runs)-1]
	m.mcp.mu.Unlock()
	if last.Profile != "staging" {
		t.Errorf("read ran through %+v", last)
	}
	text, _ := m.srv.lookupAWSAccounts(context.Background(), nil)
	if !strings.Contains(text, "read-only") || !strings.Contains(text, "primary: changes can be proposed") {
		t.Errorf("accounts:\n%s", text)
	}
	m.allowWrites("staging", true)
	text, _ = m.srv.lookupAWSAccounts(context.Background(), nil)
	if strings.Contains(text, "read-only") || !strings.Contains(text, "pass this id as account") {
		t.Errorf("accounts after:\n%s", text)
	}
}

func TestSwitchingOffOneAccountLeavesOthersWaitingProposalsAlone(t *testing.T) {
	m, _ := accountHarness(t)
	m.mcpOn()
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	addAccount(m.admin, 201, "Prod", "prod", "production")
	m.allowWrites("staging", true)
	m.allowWrites("prod", true)
	staging := m.settledChange(m.cloudChange(m.dev, 202, "staging").ID, "preparing")
	prod := m.settledChange(m.cloudChange(m.dev, 202, "prod").ID, "preparing")
	primary := m.settledChange(m.cloudChange(m.dev, 202, "").ID, "preparing")
	_, aiStaging := m.proposeFor(m.dev, "staging")
	_, aiProd := m.proposeFor(m.dev, "prod")
	_, aiPrimary := m.proposeFor(m.dev, "")

	m.allowWrites("staging", false)
	for id, want := range map[string]string{staging.ID: "canceled", prod.ID: "proposed", primary.ID: "proposed"} {
		if got := m.settledChange(id); got.Status != want {
			t.Errorf("change %s is %s, want %s", id, got.Status, want)
		}
	}
	for id, want := range map[string]string{aiStaging: apCancelled, aiProd: apProposed, aiPrimary: apProposed} {
		if got := m.waitProposal(id); got.Status != want {
			t.Errorf("proposal %s is %s, want %s", id, got.Status, want)
		}
	}
}

func TestApprovalStopsWhenWritesWereTurnedOffBehindTheScenes(t *testing.T) {
	m, st := accountHarness(t)
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	m.allowWrites("staging", true)
	ch := m.settledChange(m.cloudChange(m.dev, 202, "staging").ID, "preparing")
	var acct AWSAccount
	m.srv.docs.Get(context.Background(), kindAWSAccount, "staging", &acct)
	acct.Writes = false // not through the handler, so nothing was canceled
	m.srv.docs.Put(context.Background(), kindAWSAccount, "staging", acct)
	if b := m.admin.expect(409, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]any{}); !strings.Contains(string(b), "can no longer take changes") {
		t.Errorf("body = %s", b)
	}
	if st.called("staging", "ec2 reboot-instances") != 0 {
		t.Error("it ran")
	}
}

func TestACancelWhileThePreviewIsBeingPreparedSticks(t *testing.T) {
	m, st := accountHarness(t)
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	m.allowWrites("staging", true)
	st.gate = make(chan struct{})
	ch := m.cloudChange(m.dev, 202, "staging")
	m.waitFor("the preview to start", func() bool { return st.called("staging", "ec2 describe-instances") > 0 })
	m.allowWrites("staging", false) // while the preview is still being prepared
	close(st.gate)
	time.Sleep(300 * time.Millisecond) // let the preparing worker finish
	if got := m.settledChange(ch.ID); got.Status != "canceled" || !strings.Contains(got.DecisionNote, "turned off") {
		t.Fatalf("the worker overwrote the cancellation: %+v", got)
	}
}

func TestManualCancelDuringPreparationAlsoSticks(t *testing.T) {
	m, st := accountHarness(t)
	st.gate = make(chan struct{})
	ch := m.cloudChange(m.dev, 202, "")
	m.waitFor("the preview to start", func() bool { return st.called("dev", "ec2 describe-instances") > 0 })
	m.dev.expect(200, "POST", "/v1/cloud/changes/"+ch.ID+"/cancel", map[string]any{})
	close(st.gate)
	time.Sleep(300 * time.Millisecond)
	if got := m.settledChange(ch.ID); got.Status != "canceled" {
		t.Fatalf("a canceled change came back: %+v", got)
	}
}

func TestAssistantProposalTitlesAndAuditNameTheAccountAndDriftIsCaughtWhenFiling(t *testing.T) {
	m, st := accountHarness(t)
	m.mcpOn()
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	m.allowWrites("staging", true)
	_, id := m.proposeFor(m.dev, "staging")
	var p AIProposal
	json.Unmarshal(m.v.expect(200, "GET", "/v1/ai/proposals/"+id, nil), &p)
	if !strings.HasSuffix(p.Title, " in Staging") {
		t.Errorf("title = %q", p.Title)
	}
	if log := string(m.admin.expect(200, "GET", "/v1/audit?limit=50", nil)); !strings.Contains(log, "account Staging 444455556666") {
		t.Errorf("the audit entry does not name the account: %s", log)
	}
	st.mu.Lock()
	st.accounts["staging"] = "999900001111"
	st.mu.Unlock()
	r, id2 := m.proposeFor(m.dev, "staging")
	if told := m.llm.lastUser(1); id2 != "" || !strings.Contains(told, "now points at AWS account 999900001111") {
		t.Errorf("filed against a drifted profile: %+v, told %q", r, told)
	}
}

func TestAStalledIdentityCheckDoesNotFreezeAccountAdministration(t *testing.T) {
	m, st := accountHarness(t)
	m.mcpOn()
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	m.allowWrites("staging", true)
	_, id := m.proposeFor(m.dev, "staging")
	var p AIProposal
	json.Unmarshal(m.v.expect(200, "GET", "/v1/ai/proposals/"+id, nil), &p)

	st.stsGate = make(chan struct{})
	approved := make(chan int, 1)
	go func() {
		resp, _ := m.admin2.do("POST", "/v1/ai/proposals/"+id+"/approve", map[string]any{"hash": p.Hash})
		approved <- resp.StatusCode
	}()
	m.waitFor("the approval to reach the identity check", func() bool { return st.called("staging", "sts get-caller-identity") >= 2 })
	switched := make(chan struct{})
	go func() {
		m.allowWrites("staging", false) // needs the proposal lock; must not wait for AWS
		close(switched)
	}()
	select {
	case <-switched:
	case <-time.After(3 * time.Second):
		t.Fatal("switching writes off waited for a stalled identity check")
	}
	close(st.stsGate)
	if code := <-approved; code != 409 {
		t.Errorf("the approval of a canceled proposal answered %d", code)
	}
}

func (m *mcpHarness) switchWritesOffQuietly(id string) {
	m.t.Helper()
	var acct AWSAccount
	if err := m.srv.docs.Get(context.Background(), kindAWSAccount, id, &acct); err != nil {
		m.t.Fatal(err)
	}
	acct.Writes = false // straight to the store, as a concurrent request would between two checks
	m.srv.docs.Put(context.Background(), kindAWSAccount, id, acct)
}

func TestAChangePreparedAfterWritesWentOffIsCanceledWhenItIsSaved(t *testing.T) {
	m, st := accountHarness(t)
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	m.allowWrites("staging", true)
	st.gate = make(chan struct{})
	ch := m.cloudChange(m.dev, 202, "staging")
	m.waitFor("the preview to start", func() bool { return st.called("staging", "ec2 describe-instances") > 0 })
	m.switchWritesOffQuietly("staging")
	close(st.gate)
	m.waitFor("the worker to save", func() bool { return m.settledChange(ch.ID).Status != "preparing" })
	if got := m.settledChange(ch.ID); got.Status != "canceled" || !strings.Contains(got.DecisionNote, "has not allowed changes") {
		t.Fatalf("change = %+v", got)
	}
}

func TestAnAssistantProposalFiledAsWritesWentOffIsCanceled(t *testing.T) {
	m, st := accountHarness(t)
	m.mcpOn()
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	m.allowWrites("staging", true)
	st.stsGate = make(chan struct{})
	done := make(chan ChatReply, 1)
	go func() { r, _ := m.proposeFor(m.dev, "staging"); done <- r }()
	m.waitFor("the filing to reach the identity check", func() bool { return st.called("staging", "sts get-caller-identity") >= 2 })
	m.switchWritesOffQuietly("staging")
	close(st.stsGate)
	if r := <-done; len(r.Proposals) != 0 {
		t.Fatalf("a proposal was reported as filed: %+v", r)
	}
	var list struct{ Proposals []AIProposal }
	json.Unmarshal(m.v.expect(200, "GET", "/v1/ai/proposals", nil), &list)
	for _, p := range list.Proposals {
		if p.Status == apProposed {
			t.Errorf("a proposal for an account with changes off is still waiting: %+v", p)
		}
	}
	if len(list.Proposals) != 1 || list.Proposals[0].Status != apCancelled {
		t.Errorf("proposals = %+v", list.Proposals)
	}
}

func TestSwitchingWritesOffCleansUpAPreparedDeployChangeSet(t *testing.T) {
	m, st := accountHarness(t)
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	m.allowWrites("staging", true)
	deploy := CloudChange{ID: "deploy1", Kind: kindStackDeploy, Title: "Deploy x", Status: stProposed, Stack: "demo", ChangeSet: "cs-1", ChangeSetType: "UPDATE",
		Conn: "staging", ConnName: "Staging", Profile: "staging", Region: "eu-west-1", Account: "444455556666", ProposedBy: "dev", ProposedAt: time.Now().UTC()}
	if err := m.srv.docs.Create(context.Background(), kindCloud, deploy.ID, deploy); err != nil {
		t.Fatal(err)
	}
	m.allowWrites("staging", false)
	if got := m.settledChange(deploy.ID); got.Status != "canceled" {
		t.Fatalf("change = %+v", got)
	}
	if st.called("staging", "cloudformation delete-change-set") != 1 {
		t.Errorf("the prepared change set was left behind: %v", st.calls)
	}
}

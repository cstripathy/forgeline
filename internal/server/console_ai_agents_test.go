package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func (f *scriptedLLM) system(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, _ := f.reqs[n]["system"].(string)
	return s
}

func (f *scriptedLLM) script(replies ...string) {
	f.mu.Lock()
	f.replies, f.reqs = replies, nil
	f.mu.Unlock()
}

func newSkill(t *testing.T, c *browser, name, body string) AISkill {
	t.Helper()
	var sk AISkill
	json.Unmarshal(c.expect(201, "POST", "/v1/ai/skills", map[string]any{"name": name, "description": "d", "body": body}), &sk)
	return sk
}

func agentBody(name string, skills []SkillPin, tools []string, tier string) map[string]any {
	return map[string]any{"name": name, "description": "d", "instructions": "Review things.", "skills": skills, "tools": tools, "tier": tier}
}

func newAgent(t *testing.T, c *browser, body map[string]any) AIAgent {
	t.Helper()
	var a AIAgent
	json.Unmarshal(c.expect(201, "POST", "/v1/ai/agents", body), &a)
	return a
}

func TestSkillVersionsAreAppendOnlyAndRollbackCopiesForward(t *testing.T) {
	m := newMCPHarness(t)
	sk := newSkill(t, m.admin, "ECS rightsizing", "Step one.")
	if sk.ID != "ecs-rightsizing" || sk.Current != 1 || len(sk.Versions) != 1 || len(sk.Versions[0].Hash) != 64 {
		t.Fatalf("created = %+v", sk)
	}
	var v2 AISkill
	json.Unmarshal(m.admin.expect(201, "POST", "/v1/ai/skills/"+sk.ID+"/versions", map[string]any{"body": "Step one. Step two.", "note": "add step two"}), &v2)
	if v2.Current != 2 || len(v2.Versions) != 2 || v2.Versions[0].Body != "Step one." || v2.Versions[1].Note != "add step two" {
		t.Fatalf("after v2 = %+v", v2)
	}
	if v2.Versions[0].Hash == v2.Versions[1].Hash {
		t.Error("different content must have different hashes")
	}
	// Saving the same thing again is not a new version.
	m.admin.expect(409, "POST", "/v1/ai/skills/"+sk.ID+"/versions", map[string]any{"body": "Step one. Step two."})
	// Rolling back appends a copy; version 1 and 2 stay exactly as they were.
	var v3 AISkill
	json.Unmarshal(m.admin.expect(201, "POST", "/v1/ai/skills/"+sk.ID+"/rollback", map[string]any{"to": 1}), &v3)
	if v3.Current != 3 || len(v3.Versions) != 3 || v3.Versions[2].Body != "Step one." || !strings.Contains(v3.Versions[2].Note, "rolled back to v1") {
		t.Fatalf("after rollback = %+v", v3)
	}
	if v3.Versions[1].Body != "Step one. Step two." || v3.Versions[0].Hash != v2.Versions[0].Hash {
		t.Error("history was rewritten")
	}
	m.admin.expect(409, "POST", "/v1/ai/skills/"+sk.ID+"/rollback", map[string]any{"to": 3})
	m.admin.expect(404, "POST", "/v1/ai/skills/"+sk.ID+"/rollback", map[string]any{"to": 9})
}

func TestSkillsAndAgentsRoles(t *testing.T) {
	m := newMCPHarness(t)
	sk := newSkill(t, m.admin, "S", "body")
	for _, c := range []*browser{m.dev, m.v} {
		c.expect(403, "POST", "/v1/ai/skills", map[string]any{"name": "x", "body": "y"})
		c.expect(403, "POST", "/v1/ai/skills/"+sk.ID+"/versions", map[string]any{"body": "z"})
		c.expect(403, "POST", "/v1/ai/agents", agentBody("a", nil, nil, "T0"))
		c.expect(403, "DELETE", "/v1/ai/skills/"+sk.ID, nil)
	}
	m.dev.expect(200, "GET", "/v1/ai/skills", nil)
	m.dev.expect(200, "GET", "/v1/ai/skills/"+sk.ID, nil)
	m.v.expect(403, "GET", "/v1/ai/skills", nil)
	m.v.expect(403, "GET", "/v1/ai/agents", nil)
}

func TestAgentValidation(t *testing.T) {
	m := newMCPHarness(t)
	sk := newSkill(t, m.admin, "S", "body")
	post := func(want int, mut func(map[string]any)) string {
		b := agentBody("Reviewer", []SkillPin{{ID: sk.ID}}, []string{"aws_summary"}, "T0")
		mut(b)
		return string(m.admin.expect(want, "POST", "/v1/ai/agents", b))
	}
	for name, c := range map[string]struct {
		mut  func(map[string]any)
		want string
	}{
		"no name":         {func(b map[string]any) { b["name"] = "" }, "name"},
		"no instructions": {func(b map[string]any) { b["instructions"] = " " }, "what the agent is for"},
		"bad tier":        {func(b map[string]any) { b["tier"] = "T9" }, "tier"},
		"unknown tool":    {func(b map[string]any) { b["tools"] = []string{"rm_rf"} }, "not a tool"},
		"agents calling":  {func(b map[string]any) { b["tools"] = []string{"ask_agent"} }, "cannot call other agents"},
		"propose on T0":   {func(b map[string]any) { b["tools"] = []string{"propose_aws_change"} }, "review-tier"},
		"duplicate tool":  {func(b map[string]any) { b["tools"] = []string{"get_run", "get_run"} }, "twice"},
		"unknown skill":   {func(b map[string]any) { b["skills"] = []SkillPin{{ID: "ghost"}} }, "no skill"},
		"skill version":   {func(b map[string]any) { b["skills"] = []SkillPin{{ID: sk.ID, N: 7}} }, "no version 7"},
		"dup skill":       {func(b map[string]any) { b["skills"] = []SkillPin{{ID: sk.ID}, {ID: sk.ID}} }, "twice"},
	} {
		if msg := post(400, c.mut); !strings.Contains(msg, c.want) {
			t.Errorf("%s: %s", name, msg)
		}
	}
	// A review-tier agent may be given the proposal tool.
	b := agentBody("Changer", nil, []string{"propose_aws_change", "aws_summary"}, "T1")
	m.admin.expect(201, "POST", "/v1/ai/agents", b)
	// At most eight skills.
	var many []SkillPin
	for i := 0; i < maxAgentSkills+1; i++ {
		many = append(many, SkillPin{ID: newSkill(t, m.admin, "Skill "+string(rune('A'+i)), "x").ID})
	}
	if msg := post(400, func(b map[string]any) { b["skills"] = many }); !strings.Contains(msg, "at most") {
		t.Errorf("too many skills: %s", msg)
	}
}

func TestAgentPinsSkillVersionsSoARunIsReproducible(t *testing.T) {
	m := newMCPHarness(t)
	m.srv.run = (&multiStub{accounts: map[string]string{"dev": "111122223333"}}).run
	sk := newSkill(t, m.admin, "Checklist", "OLD-PROCEDURE")
	a := newAgent(t, m.admin, agentBody("Reviewer", []SkillPin{{ID: sk.ID}}, []string{"get_run"}, "T0"))
	if a.Versions[0].Skills[0].N != 1 {
		t.Fatalf("an unpinned skill should be pinned to its current version: %+v", a.Versions[0].Skills)
	}
	m.admin.expect(201, "POST", "/v1/ai/skills/"+sk.ID+"/versions", map[string]any{"body": "NEW-PROCEDURE", "note": "better"})

	m.llm.script("answer one")
	m.dev.expect(200, "POST", "/v1/ai/agents/"+a.ID+"/chat", map[string]any{"message": "go"})
	sys := m.llm.system(0)
	if !strings.Contains(sys, "OLD-PROCEDURE") || strings.Contains(sys, "NEW-PROCEDURE") {
		t.Errorf("the agent version should still use the skill version it pinned: %s", sys)
	}
	// A new agent version with the pin left open picks up the new skill version.
	var a2 AIAgent
	json.Unmarshal(m.admin.expect(201, "POST", "/v1/ai/agents/"+a.ID+"/versions", agentBody("Reviewer", []SkillPin{{ID: sk.ID}}, []string{"get_run"}, "T0")), &a2)
	if a2.Current != 2 || a2.Versions[1].Skills[0].N != 2 {
		t.Fatalf("v2 = %+v", a2.Versions)
	}
	m.llm.script("answer two")
	m.dev.expect(200, "POST", "/v1/ai/agents/"+a.ID+"/chat", map[string]any{"message": "go"})
	if sys := m.llm.system(0); !strings.Contains(sys, "NEW-PROCEDURE") || !strings.Contains(sys, "version 2") {
		t.Errorf("current version should use the new skill: %s", sys)
	}
	// An earlier version can still be run exactly.
	m.llm.script("answer three")
	var out map[string]any
	json.Unmarshal(m.dev.expect(200, "POST", "/v1/ai/agents/"+a.ID+"/chat", map[string]any{"message": "go", "version": 1}), &out)
	if !strings.Contains(m.llm.system(0), "OLD-PROCEDURE") || out["agent"] != a.ID+"@1" || out["hash"] != a.Versions[0].Hash {
		t.Errorf("version 1 run: %v", out)
	}
	m.dev.expect(404, "POST", "/v1/ai/agents/"+a.ID+"/chat", map[string]any{"message": "go", "version": 9})
}

func TestAgentCanUseOnlyItsOwnTools(t *testing.T) {
	m := newMCPHarness(t)
	a := newAgent(t, m.admin, agentBody("Run reader", nil, []string{"get_run"}, "T0"))
	m.llm.script(lookupReply("list_pipelines", map[string]any{}), "done")
	var out map[string]any
	json.Unmarshal(m.dev.expect(200, "POST", "/v1/ai/agents/"+a.ID+"/chat", map[string]any{"message": "go"}), &out)
	if res := m.llm.lastUser(1); !strings.Contains(res, "not one of this agent's tools") {
		t.Errorf("a tool outside the allowlist ran: %s", res)
	}
	help := m.llm.system(0)
	if !strings.Contains(help, "get_run") || strings.Contains(help, "list_pipelines") || strings.Contains(help, "ask_agent") || strings.Contains(help, "aws_summary") {
		t.Errorf("the agent was shown tools it may not use: %s", help)
	}
	// The plain assistant is unaffected by agents.
	m.llm.script("hello")
	m.dev.expect(200, "POST", "/v1/assistant/chat", map[string]any{"message": "hi"})
	if sys := m.llm.system(0); !strings.Contains(sys, "list_pipelines") || !strings.Contains(sys, "ask_agent") || strings.Contains(sys, "Run reader") {
		t.Errorf("assistant prompt: %s", sys)
	}
}

func TestAgentNeverGainsThePersonsPermissions(t *testing.T) {
	m := newMCPHarness(t)
	// call_aws is administrators only. An agent that lists it still runs it as the developer asking.
	a := newAgent(t, m.admin, agentBody("Caller", nil, []string{"call_aws"}, "T0"))
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})
	m.llm.script(lookupReply("call_aws", map[string]any{"cli_command": "aws s3api list-buckets"}), "done")
	m.dev.expect(200, "POST", "/v1/ai/agents/"+a.ID+"/chat", map[string]any{"message": "go"})
	if res := m.llm.lastUser(1); !strings.Contains(res, "not available to your role") {
		t.Errorf("a developer's agent run used an admin tool: %s", res)
	}
}

func TestAssistantDelegatesToAnAgentButAgentsCannotDelegate(t *testing.T) {
	m := newMCPHarness(t)
	a := newAgent(t, m.admin, agentBody("Cost watcher", nil, []string{"get_run"}, "T0"))
	// assistant asks the agent; the agent answers; the assistant gives the final reply.
	m.llm.script(lookupReply("ask_agent", map[string]any{"agent": a.ID, "task": "look at spend"}), "agent says: spend is flat", "Final: spend is flat.")
	var r ChatReply
	json.Unmarshal(m.dev.expect(200, "POST", "/v1/assistant/chat", map[string]any{"message": "how is spend?"}), &r)
	if r.Reply != "Final: spend is flat." || len(r.Lookups) != 1 || r.Lookups[0] != "ask_agent" {
		t.Fatalf("reply = %+v", r)
	}
	if res := m.llm.lastUser(2); !strings.Contains(res, "Answer from agent "+a.ID+"@1") || !strings.Contains(res, "spend is flat") {
		t.Errorf("the assistant was not given the agent's answer: %s", res)
	}
	if sys := m.llm.system(1); !strings.Contains(sys, "Cost watcher") {
		t.Errorf("the agent did not run with its own prompt: %s", sys)
	}
	// An agent that tries to call another agent is refused.
	m.llm.script(lookupReply("ask_agent", map[string]any{"agent": a.ID, "task": "loop"}), lookupReply("ask_agent", map[string]any{"agent": a.ID, "task": "loop"}), "gave up", "Final.")
	m.dev.expect(200, "POST", "/v1/assistant/chat", map[string]any{"message": "again"})
	if res := m.llm.lastUser(2); !strings.Contains(res, "not one of this agent's tools") {
		t.Errorf("an agent delegated: %s", res)
	}
	// list_agents shows it, and a disabled agent is hidden and cannot run.
	m.admin.expect(200, "POST", "/v1/ai/agents/"+a.ID+"/disable", nil)
	m.dev.expect(409, "POST", "/v1/ai/agents/"+a.ID+"/chat", map[string]any{"message": "go"})
	m.llm.script(lookupReply("list_agents", map[string]any{}), "done")
	m.dev.expect(200, "POST", "/v1/assistant/chat", map[string]any{"message": "who?"})
	if res := m.llm.lastUser(1); !strings.Contains(res, "There are no agents") {
		t.Errorf("disabled agent listed: %s", res)
	}
}

func TestAgentRunsAreAudited(t *testing.T) {
	m := newMCPHarness(t)
	a := newAgent(t, m.admin, agentBody("Auditor", nil, nil, "T0"))
	m.llm.script("ok")
	m.dev.expect(200, "POST", "/v1/ai/agents/"+a.ID+"/chat", map[string]any{"message": "go"})
	log := string(m.admin.expect(200, "GET", "/v1/audit?limit=50", nil))
	for _, want := range []string{"ai.agent.create", "ai.agent.run", "agent/" + a.ID} {
		if !strings.Contains(log, want) {
			t.Errorf("audit log has no %q", want)
		}
	}
}

func TestSkillInUseCannotBeDeletedAndAgentRollbackCopiesForward(t *testing.T) {
	m := newMCPHarness(t)
	sk := newSkill(t, m.admin, "S", "body")
	a := newAgent(t, m.admin, agentBody("A", []SkillPin{{ID: sk.ID}}, nil, "T0"))
	m.admin.expect(409, "DELETE", "/v1/ai/skills/"+sk.ID, nil)
	b := agentBody("A", []SkillPin{{ID: sk.ID}}, []string{"get_run"}, "T0")
	m.admin.expect(201, "POST", "/v1/ai/agents/"+a.ID+"/versions", b)
	m.admin.expect(409, "POST", "/v1/ai/agents/"+a.ID+"/versions", b) // unchanged
	var back AIAgent
	json.Unmarshal(m.admin.expect(201, "POST", "/v1/ai/agents/"+a.ID+"/rollback", map[string]any{"to": 1}), &back)
	if back.Current != 3 || len(back.Versions) != 3 || len(back.Versions[2].Tools) != 0 || back.Versions[1].Tools[0] != "get_run" {
		t.Errorf("rollback = %+v", back.Versions)
	}
	if back.Versions[2].Hash == back.Versions[0].Hash {
		t.Error("a rolled-back copy is its own version (its number is part of the hash)")
	}
	free := newSkill(t, m.admin, "Unused", "x")
	m.admin.expect(204, "DELETE", "/v1/ai/skills/"+free.ID, nil)
	m.admin.expect(404, "GET", "/v1/ai/skills/"+free.ID, nil)
}

// Even if a stored agent somehow listed ask_agent (a hand-edited document, say), a running agent
// still cannot reach another agent. Each layer is checked alone.
func TestAgentNestingIsRefusedByEveryLayer(t *testing.T) {
	m := newMCPHarness(t)
	a := newAgent(t, m.admin, agentBody("Inner", nil, nil, "T0"))
	scope := &agentScope{tools: map[string]bool{askAgentTool: true, "get_run": true}, label: "x@1"}
	ctx := context.WithValue(context.Background(), agentScopeKey{}, scope)
	if _, err := m.srv.lookupAskAgent(ctx, json.RawMessage(`{"agent":"`+a.ID+`","task":"x"}`)); err == nil || !strings.Contains(err.Error(), "cannot call other agents") {
		t.Errorf("lookupAskAgent inside an agent: %v", err)
	}
	if _, err := m.srv.runLookup(ctx, Principal{Name: "dev", Role: "admin"}, lookupCall{Tool: askAgentTool, Args: json.RawMessage(`{"agent":"` + a.ID + `","task":"x"}`)}); err == nil || !strings.Contains(err.Error(), "not one of this agent's tools") {
		t.Errorf("runLookup inside an agent: %v", err)
	}
	if strings.Contains(m.srv.lookupHelp(ctx, "admin"), "ask_agent") {
		t.Error("an agent is shown ask_agent")
	}
}

func TestAIToolsListHidesAskAgentAndMarksMCPTools(t *testing.T) {
	m := newMCPHarness(t)
	var out struct {
		Tools []struct{ Name, Min, Needs string }
	}
	json.Unmarshal(m.dev.expect(200, "GET", "/v1/ai/tools", nil), &out)
	got := map[string]string{}
	for _, x := range out.Tools {
		got[x.Name] = x.Needs
	}
	if _, ok := got["ask_agent"]; ok {
		t.Error("ask_agent must not be offered to an agent")
	}
	if got["aws_script"] != "mcp" || got["get_run"] != "" {
		t.Errorf("tools = %v", got)
	}
	m.v.expect(403, "GET", "/v1/ai/tools", nil)
}

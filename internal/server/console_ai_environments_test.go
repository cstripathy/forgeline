package server

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	awsdeploy "github.com/forgelinehq/forgeline/deploy/aws"
	"github.com/forgelinehq/forgeline/internal/authn"
)

func (c *cloudHarness) lookupAs(name string, role authn.Role, scope *agentScope, tool string, args map[string]any) (string, error) {
	c.t.Helper()
	raw, _ := json.Marshal(args)
	ctx := context.WithValue(context.Background(), principalKey{}, Principal{Name: name, Role: role})
	if scope != nil {
		ctx = context.WithValue(ctx, agentScopeKey{}, scope)
	}
	ctx = context.WithValue(ctx, lookupSinkKey{}, &lookupSink{})
	return c.srv.runLookup(ctx, Principal{Name: name, Role: role}, lookupCall{Tool: tool, Args: raw})
}

func (c *cloudHarness) envByName(name string) Environment {
	c.t.Helper()
	var out struct{ Environments []Environment }
	json.Unmarshal(c.dev.expect(200, "GET", "/v1/envs", nil), &out)
	for _, e := range out.Environments {
		if e.Name == name {
			return e
		}
	}
	c.t.Fatalf("no environment named %q in %+v", name, out.Environments)
	return Environment{}
}

func TestAssistantRequestsAnEnvironmentButNeverStartsOne(t *testing.T) {
	c := envHarness(t)
	// Self-service is on for this template, so a person's own request would start by itself.
	c.setPolicy(selfService("qa-sandbox"))
	res, err := c.lookupAs("dev", authn.Developer, nil, "request_environment", map[string]any{"template": "qa-sandbox", "name": "Queue check", "purpose": "Trying the queue consumer"})
	if err != nil || !strings.Contains(res, "Nothing exists yet") || !strings.Contains(res, "different administrator") {
		t.Fatalf("res = %q, err = %v", res, err)
	}
	e := c.envByName("Queue check")
	if !e.ViaAI || e.AutoApprove || e.Owner != "dev" || e.Template != "qa-sandbox" || e.TTLHours != 24 || e.Status != envRequested {
		t.Fatalf("env = %+v", e)
	}
	// The engine runs, and still nothing is approved: the policy does not apply to what an AI asked for.
	for i := 0; i < 5; i++ {
		c.srv.envTick(context.Background(), time.Now())
		time.Sleep(10 * time.Millisecond)
	}
	ch := c.waitChange(e.ChangeID, "preparing")
	if ch.Status != stProposed || !strings.Contains(ch.Reason, "Asked for by the assistant") || ch.ProposedBy != "dev" {
		t.Fatalf("change = %+v", ch)
	}
	if e = c.env(e.ID); e.Status != envRequested {
		t.Fatalf("it started on its own: %+v", e)
	}
	// The person it is for cannot approve it either; a different administrator can.
	c.dev.expect(403, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]any{})
	c.admin2.expect(202, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]any{})
	if e = c.waitEnv(e.ID, envActive, envFailed); e.Status != envActive || !e.ViaAI {
		t.Fatalf("after approval: %+v", e)
	}
	if log := string(c.admin.expect(200, "GET", "/v1/audit?limit=100", nil)); !strings.Contains(log, "env.request") || !strings.Contains(log, "asked for by the assistant") {
		t.Errorf("audit log: %s", log)
	}
	// The same request typed in by the person still starts under the policy: nothing about that changed.
	person := c.requestEnv(c.login("dev2"), 202, map[string]any{"template": "qa-sandbox", "name": "By hand", "ttl_hours": 8})
	if !person.AutoApprove || person.ViaAI {
		t.Errorf("a person's own request: %+v", person)
	}
}

func TestAssistantEnvironmentRequestRules(t *testing.T) {
	c := envHarness(t)
	ask := func(name string, role authn.Role, args map[string]any) (string, error) {
		return c.lookupAs(name, role, nil, "request_environment", args)
	}
	good := map[string]any{"template": "qa-sandbox", "name": "Rules check", "purpose": "Exercising the rules"}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range good {
			m[a] = b
		}
		m[k] = v
		return m
	}
	for name, tc := range map[string]struct {
		who  string
		role authn.Role
		args map[string]any
		want string
	}{
		"a viewer":             {"view", authn.Viewer, good, "not available to your role"},
		"a scheduled workflow": {"workflow:nightly", authn.Developer, good, "only a person can own"},
		"a policy actor":       {"policy:expiry", authn.Admin, good, "only a person can own"},
		"unknown template":     {"dev", authn.Developer, with("template", "mainframe"), "environment_templates lists"},
		"a bad name":           {"dev", authn.Developer, with("name", "x"), "3 to 40"},
		"no purpose":           {"dev", authn.Developer, with("purpose", "hi"), "5 to 300"},
		"too long a life":      {"dev", authn.Developer, with("ttl_hours", 5000), "between 1 and"},
		"a setting it hasn't":  {"dev", authn.Developer, with("params", map[string]string{"Owner": "someone-else"}), "not something you can set"},
	} {
		if _, err := ask(tc.who, tc.role, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	// A disabled account is not a person who can own one either.
	c.admin.expect(200, "PATCH", "/v1/users/dev2", map[string]any{"disabled": true})
	if _, err := ask("dev2", authn.Developer, good); err == nil || !strings.Contains(err.Error(), "only a person can own") {
		t.Errorf("a disabled account: %v", err)
	}
	c.admin.expect(200, "PATCH", "/v1/users/dev2", map[string]any{"disabled": false})
	var out struct{ Environments []Environment }
	json.Unmarshal(c.dev.expect(200, "GET", "/v1/envs", nil), &out)
	if len(out.Environments) != 0 {
		t.Errorf("a refused request left environments behind: %+v", out.Environments)
	}
	// The per-person limit counts what the assistant asked for, so it cannot pile up requests.
	p := selfService("qa-sandbox")
	p.MaxActivePerUser = 1
	c.setPolicy(p)
	if _, err := ask("dev", authn.Developer, good); err != nil {
		t.Fatal(err)
	}
	if _, err := ask("dev", authn.Developer, with("name", "Second one")); err == nil || !strings.Contains(err.Error(), "limit is 1") {
		t.Errorf("limit: %v", err)
	}
	// Fields the tool does not have are ignored: there is nothing to say "start it yourself" with.
	c.setPolicy(selfService("qa-sandbox"))
	extra := with("auto_approve", true)
	extra["name"] = "Extra field ignored"
	if _, err := c.lookupAs("dev2", authn.Developer, nil, "request_environment", extra); err != nil {
		t.Fatal(err)
	}
	var all struct{ Environments []Environment }
	json.Unmarshal(c.dev.expect(200, "GET", "/v1/envs", nil), &all)
	for _, e := range all.Environments {
		if e.AutoApprove || !e.ViaAI {
			t.Errorf("an AI request started on its own: %+v", e)
		}
	}
	// The assistant never asks in someone else's name.
	if e := c.envByName("Rules check"); e.Owner != "dev" {
		t.Errorf("owner = %s", e.Owner)
	}
}

func TestAssistantCanListTemplatesAndOnlyAnAgentWithTheToolMayRequest(t *testing.T) {
	c := envHarness(t)
	out, err := c.lookupAs("dev", authn.Developer, nil, "environment_templates", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"dev-box", "qa-sandbox", "lambda-sandbox", "event-pipeline", "workflow-sandbox"} {
		if !strings.Contains(out, "- "+id+":") {
			t.Errorf("environment_templates is missing %s: %s", id, out)
		}
	}
	if !strings.Contains(out, "InstanceType (one of t4g.small, t4g.medium) default t4g.small") || !strings.Contains(out, "always approved by a person") {
		t.Errorf("settings or the approval rule are missing: %s", out)
	}
	// An agent sees only its own tools.
	args := map[string]any{"template": "qa-sandbox", "name": "Agent asked", "purpose": "An agent needs a queue"}
	narrow := &agentScope{tools: map[string]bool{"environment_templates": true}, label: "a@1"}
	if _, err := c.lookupAs("dev", authn.Developer, narrow, "request_environment", args); err == nil || !strings.Contains(err.Error(), "not one of this agent's tools") {
		t.Errorf("an agent without the tool: %v", err)
	}
	if help := c.srv.lookupHelp(context.WithValue(context.Background(), agentScopeKey{}, narrow), authn.Developer); strings.Contains(help, "request_environment") || !strings.Contains(help, "environment_templates") {
		t.Errorf("help for a narrow agent: %s", help)
	}
	wide := &agentScope{tools: map[string]bool{"request_environment": true}, label: "a@1"}
	if _, err := c.lookupAs("dev", authn.Developer, wide, "request_environment", args); err != nil {
		t.Errorf("an agent with the tool: %v", err)
	}
	// A viewer sees neither tool.
	if help := c.srv.lookupHelp(context.Background(), authn.Viewer); strings.Contains(help, "request_environment") || strings.Contains(help, "environment_templates") {
		t.Errorf("a viewer's help lists environment tools: %s", help)
	}
}

func TestOnlyAReviewTierAgentMayAskForEnvironments(t *testing.T) {
	m := newMCPHarness(t)
	for _, tc := range []struct {
		tier string
		want int
	}{{"T0", 400}, {"T1", 201}} {
		body := agentBody("Env "+tc.tier, nil, []string{"request_environment", "environment_templates"}, tc.tier)
		out := string(m.admin.expect(tc.want, "POST", "/v1/ai/agents", body))
		if tc.want == 400 && !strings.Contains(out, "review-tier") {
			t.Errorf("T0: %s", out)
		}
	}
}

func TestNewEnvironmentTemplatesAreInTheCatalogWithPrices(t *testing.T) {
	c := envHarness(t)
	var out struct {
		Templates []struct {
			ID         string
			Kind       string
			MonthlyUSD float64 `json:"monthly_usd"`
			DefaultTTL int     `json:"default_ttl_hours"`
			Params     []CatalogParam
		}
	}
	json.Unmarshal(c.dev.expect(200, "GET", "/v1/envs/templates", nil), &out)
	want := map[string]float64{"lambda-sandbox": 0.5, "event-pipeline": 1, "workflow-sandbox": 0.5}
	seen := 0
	for _, tp := range out.Templates {
		if price, ok := want[tp.ID]; ok {
			seen++
			if tp.MonthlyUSD != price || tp.DefaultTTL != 24 || len(tp.Params) != 0 || tp.Kind == "" {
				t.Errorf("%s = %+v", tp.ID, tp)
			}
		}
	}
	if seen != 3 {
		t.Fatalf("found %d of the 3 new templates", seen)
	}
	// Each one goes through the ordinary request path and is validated as a template before it is proposed.
	for id := range want {
		e := c.requestEnv(c.dev, 202, map[string]any{"template": id, "name": "Try " + id, "ttl_hours": 4})
		ch := c.waitChange(e.ChangeID, "preparing")
		if ch.Status != stProposed {
			t.Errorf("%s: %+v", id, ch)
		}
		c.dev.expect(200, "POST", "/v1/cloud/changes/"+e.ChangeID+"/cancel", map[string]any{})
		c.waitEnv(e.ID, envRejected)
	}
}

func TestEnvironmentRequestsAreReportedToThePersonByTheAssistantAndAgents(t *testing.T) {
	m := newMCPHarness(t)
	m.turnOn()
	args := map[string]any{"template": "qa-sandbox", "name": "Chat asked", "purpose": "Testing a consumer"}
	r := m.ask(m.dev, lookupReply("request_environment", args))
	if len(r.Environments) != 1 || !strings.Contains(m.llm.lastUser(1), "Nothing exists yet") {
		t.Fatalf("the assistant's reply should carry the environment: %+v / %s", r, m.llm.lastUser(1))
	}
	// The same through a review-tier agent, run by a different person (each may have one at a time).
	a := newAgent(t, m.admin, agentBody("Requester", nil, []string{"request_environment"}, "T1"))
	args["name"] = "Agent asked"
	m.llm.script(lookupReply("request_environment", args), "done")
	var out struct{ Environments []string }
	json.Unmarshal(m.admin2.expect(200, "POST", "/v1/ai/agents/"+a.ID+"/chat", map[string]any{"message": "go"}), &out)
	if len(out.Environments) != 1 {
		t.Errorf("the agent's reply should carry the environment: %+v", out)
	}
}

func TestAssistantEnvironmentArgumentsMayBeLooselyTyped(t *testing.T) {
	c := envHarness(t)
	// Models write numbers as strings and settings as numbers; both are understood.
	res, err := c.srv.runLookup(context.WithValue(context.Background(), principalKey{}, Principal{Name: "dev", Role: authn.Developer}), Principal{Name: "dev", Role: authn.Developer},
		lookupCall{Tool: "request_environment", Args: json.RawMessage(`{"template":"dev-box","name":"Loose types","purpose":"Trying loose argument types","ttl_hours":"6","params":{"DiskGiB":50,"InstanceType":"t4g.medium"}}`)})
	if err != nil || !strings.Contains(res, "for 6 hours") {
		t.Fatalf("res = %q, err = %v", res, err)
	}
	if e := c.envByName("Loose types"); e.TTLHours != 6 || e.Params["DiskGiB"] != "50" || e.Params["InstanceType"] != "t4g.medium" {
		t.Errorf("env = %+v", e)
	}
	for _, bad := range []string{`"ttl_hours":"soon"`, `"ttl_hours":1.5`, `"ttl_hours":-3`, `"ttl_hours":[8]`, `"params":{"DiskGiB":[1]}`, `"params":{"DiskGiB":{"a":1}}`} {
		raw := `{"template":"qa-sandbox","name":"Bad types","purpose":"Trying bad argument types",` + bad + `}`
		if _, err := c.srv.runLookup(context.WithValue(context.Background(), principalKey{}, Principal{Name: "dev2", Role: authn.Developer}), Principal{Name: "dev2", Role: authn.Developer},
			lookupCall{Tool: "request_environment", Args: json.RawMessage(raw)}); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

func TestEnvironmentReasonKeepsTheAssistantNoteAndFitsInBytes(t *testing.T) {
	// The longest request the form allows: a 40-character name, a 300-character purpose and a 64-character owner.
	req := envRequest{Name: strings.Repeat("n", 40), Purpose: strings.Repeat("p", 300), TTLHours: 24}
	owner := strings.Repeat("o", 64)
	raw := envReason(owner, req, "Event pipeline", 0.03, true)
	if len(raw) <= 500 {
		t.Fatalf("the worst case should overflow, got %d bytes", len(raw))
	}
	got := fitBytes(raw, 500)
	if len(got) > 500 || !strings.HasPrefix(got, "Asked for by the assistant") || !utf8.ValidString(got) {
		t.Errorf("%d bytes: %.80s", len(got), got)
	}
	if h := fitBytes(envReason(owner, req, "Event pipeline", 0.03, false), 500); strings.HasPrefix(h, "Asked for") {
		t.Errorf("a person's request carries the assistant's note: %.60s", h)
	}
	// Cutting never splits a character.
	for _, in := range []string{strings.Repeat("é", 400), strings.Repeat("日", 300), "a" + strings.Repeat("😀", 200)} {
		if out := fitBytes(in, 500); len(out) > 500 || !utf8.ValidString(out) {
			t.Errorf("fitBytes(%d bytes) = %d bytes, valid %v", len(in), len(out), utf8.ValidString(out))
		}
	}
	if fitBytes("short", 500) != "short" {
		t.Error("a short reason must pass through unchanged")
	}
	// End to end, a long non-ASCII purpose is accepted and the stored reason is valid and within the limit.
	c := envHarness(t)
	long := strings.Repeat("é", 140) // 280 bytes
	p := c.requestEnv(c.login("dev2"), 202, map[string]any{"template": "qa-sandbox", "name": "Long by hand", "purpose": long})
	ch := c.waitChange(p.ChangeID, "preparing")
	if ch.Status != stProposed || len(ch.Reason) > 500 || !utf8.ValidString(ch.Reason) {
		t.Errorf("a person's reason: %d bytes: %.80s", len(ch.Reason), ch.Reason)
	}
}

func TestAssistantDefaultLifetimeStaysWithinThePolicy(t *testing.T) {
	c := envHarness(t)
	p := selfService("qa-sandbox")
	p.MaxLifetimeHours, p.MaxTTLHours = 8, 8
	c.setPolicy(p)
	if _, err := c.lookupAs("dev", authn.Developer, nil, "request_environment", map[string]any{"template": "qa-sandbox", "name": "Short life", "purpose": "No lifetime given"}); err != nil {
		t.Fatal(err)
	}
	if e := c.envByName("Short life"); e.TTLHours != 8 {
		t.Errorf("ttl = %d, want the policy's 8", e.TTLHours)
	}
	out, _ := c.lookupAs("dev", authn.Developer, nil, "environment_templates", map[string]any{})
	if !strings.Contains(out, "usual lifetime 8 hours") || strings.Contains(out, "usual lifetime 24 hours") {
		t.Errorf("environment_templates: %s", out)
	}
}

func TestEventPipelineDeadLettersOutliveTheSourceQueue(t *testing.T) {
	body := awsdeploy.EventPipelineTemplate
	retention := func(resource string) int {
		i := strings.Index(body, "\n  "+resource+":")
		if i < 0 {
			t.Fatalf("no %s", resource)
		}
		rest := body[i+1:]
		if j := strings.Index(rest[1:], "\n  "); j >= 0 {
			if k := regexp.MustCompile(`\n  [A-Za-z]+:\n`).FindStringIndex(rest[1:]); k != nil {
				rest = rest[:k[0]+1]
			}
		}
		m := regexp.MustCompile(`MessageRetentionPeriod: (\d+)`).FindStringSubmatch(rest)
		if m == nil {
			t.Fatalf("%s has no retention", resource)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	if dlq, src := retention("DeadLetters"), retention("Queue"); dlq <= src {
		t.Errorf("dead-letter retention %d must exceed the source queue's %d", dlq, src)
	}
}

func TestEnvironmentTemplatesDoNotShareParameterSlices(t *testing.T) {
	var shared []*CatalogParam
	for _, id := range []string{"qa-sandbox", "lambda-sandbox", "event-pipeline", "workflow-sandbox"} {
		tpl, ok := findTemplate(id)
		if !ok {
			t.Fatalf("no %s", id)
		}
		shared = append(shared, &tpl.Parameters[0])
	}
	for i := range shared {
		for j := i + 1; j < len(shared); j++ {
			if shared[i] == shared[j] {
				t.Errorf("templates %d and %d share their parameter slice", i, j)
			}
		}
	}
}

func TestWorkflowStepsRecordTheEnvironmentsTheyAskedFor(t *testing.T) {
	m := newMCPHarness(t)
	m.turnOn()
	a := newAgent(t, m.admin, agentBody("Requester", nil, []string{"request_environment"}, "T1"))
	w := newWorkflow(t, m.admin, wfBody("Asks", []WFStage{{Steps: []WFStep{step(a.ID, "get me a queue")}}}))
	m.llm.script(lookupReply("request_environment", map[string]any{"template": "qa-sandbox", "name": "From a workflow", "purpose": "Testing a consumer"}), "asked")
	run := m.waitRun(m.startWF(m.dev, w.ID, 202).ID, wfSucceeded, wfFailed)
	if run.Status != wfSucceeded || len(run.Environments) != 1 || len(run.Stages) == 0 || len(run.Stages[0].Steps) == 0 || len(run.Stages[0].Steps[0].Environments) != 1 {
		t.Fatalf("run = %+v", run)
	}
	var out struct{ Environments []Environment }
	json.Unmarshal(m.dev.expect(200, "GET", "/v1/envs", nil), &out)
	if len(out.Environments) != 1 || out.Environments[0].ID != run.Environments[0] || out.Environments[0].Owner != "dev" || out.Environments[0].AutoApprove {
		t.Errorf("environments = %+v", out.Environments)
	}
}

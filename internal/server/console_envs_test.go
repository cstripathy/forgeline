package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/forgelinehq/forgeline/internal/authn"
)

func envHarness(t *testing.T) *cloudHarness {
	c := newCloudHarness(t)
	c.connectAWS()
	c.admin.createUser("dev2", authn.Developer)
	return c
}

func (c *cloudHarness) setPolicy(p EnvPolicy) {
	c.t.Helper()
	c.admin.expect(200, "PUT", "/v1/envs/policy", p)
}

func selfService(templates ...string) EnvPolicy {
	return EnvPolicy{SelfService: true, Templates: templates, MaxTTLHours: 24, MaxLifetimeHours: 72, MaxActivePerUser: 2, MaxMonthlyUSD: 40}
}

func (c *cloudHarness) requestEnv(b *browser, want int, body map[string]any) Environment {
	c.t.Helper()
	req := map[string]any{"template": "dev-box", "name": "Checkout work", "purpose": "Trying the checkout branch", "ttl_hours": 8}
	for k, v := range body {
		req[k] = v
	}
	var e Environment
	json.Unmarshal(b.expect(want, "POST", "/v1/envs", req), &e)
	return e
}

func (c *cloudHarness) env(id string) Environment {
	c.t.Helper()
	var e Environment
	json.Unmarshal(c.dev.expect(200, "GET", "/v1/envs/"+id, nil), &e)
	return e
}

// waitEnv runs the engine until the environment reaches one of the statuses.
func (c *cloudHarness) waitEnv(id string, statuses ...string) Environment {
	c.t.Helper()
	var e Environment
	c.waitFor("environment "+id+" to be "+strings.Join(statuses, " or "), func() bool {
		c.srv.envTick(context.Background(), time.Now())
		e = c.env(id)
		for _, s := range statuses {
			if e.Status == s {
				return true
			}
		}
		return false
	})
	return e
}

func TestEnvTemplatesAndEstimates(t *testing.T) {
	c := envHarness(t)
	var out struct {
		Templates []struct {
			ID         string
			MonthlyUSD float64 `json:"monthly_usd"`
			Params     []CatalogParam
		}
	}
	json.Unmarshal(c.dev.expect(200, "GET", "/v1/envs/templates", nil), &out)
	got := map[string]float64{}
	for _, tp := range out.Templates {
		got[tp.ID] = tp.MonthlyUSD
		for _, p := range tp.Params {
			if p.Name == "Owner" || p.Name == "VpcId" || p.Name == "ExpiresAt" {
				t.Errorf("%s offers %s, which the server decides", tp.ID, p.Name)
			}
		}
	}
	if !strings.Contains(string(c.dev.expect(200, "GET", "/v1/envs/templates", nil)), `"params":[]`) {
		t.Error("a template with no options must send an empty list, which the console maps over")
	}
	if len(got) != 2 || got["dev-box"] != 20.65 || got["qa-sandbox"] != 1 || got["forgeline-demo"] != 0 {
		t.Errorf("templates = %v", got)
	}
	c.view.expect(403, "GET", "/v1/envs/templates", nil)

	est := func(tpl string, ttl int, params map[string]string) envQuote {
		var q envQuote
		json.Unmarshal(c.dev.expect(200, "POST", "/v1/envs/estimate", map[string]any{"template": tpl, "ttl_hours": ttl, "params": params}), &q)
		return q
	}
	if q := est("dev-box", 8, nil); q.SelfService || !strings.Contains(q.Why, "Self-service is off") || q.MonthlyUSD != 20.65 || q.ForTTLUSD != 0.23 {
		t.Errorf("policy off: %+v", q)
	}
	c.setPolicy(selfService("dev-box"))
	if q := est("dev-box", 8, nil); !q.SelfService || q.Why != "" {
		t.Errorf("within policy: %+v", q)
	}
	if q := est("qa-sandbox", 8, nil); q.SelfService || !strings.Contains(q.Why, "not on the self-service list") {
		t.Errorf("template not listed: %+v", q)
	}
	if q := est("dev-box", 48, nil); q.SelfService || !strings.Contains(q.Why, "at most 24 hours") {
		t.Errorf("too long: %+v", q)
	}
	if q := est("dev-box", 8, map[string]string{"InstanceType": "t4g.medium", "DiskGiB": "100"}); q.SelfService || !strings.Contains(q.Why, "over the self-service limit") || q.MonthlyUSD != 41.65 {
		t.Errorf("too costly: %+v", q)
	}
	c.dev.expect(400, "POST", "/v1/envs/estimate", map[string]any{"template": "forgeline-demo", "ttl_hours": 8})
	c.dev.expect(400, "POST", "/v1/envs/estimate", map[string]any{"template": "dev-box", "ttl_hours": 0})
	c.dev.expect(400, "POST", "/v1/envs/estimate", map[string]any{"template": "dev-box", "ttl_hours": 8, "params": map[string]string{"DiskGiB": "5"}})
}

func TestEnvWithoutSelfServiceWaitsForAnotherAdministrator(t *testing.T) {
	c := envHarness(t)
	e := c.requestEnv(c.dev, 202, nil)
	if e.Status != envRequested || e.AutoApprove || e.Owner != "dev" || !strings.HasPrefix(e.Stack, "env-checkout-work-") || e.Params["VpcId"] != "vpc-0def0def0def0def0" || e.Params["SubnetId"] != "subnet-0aaa0aaa0aaa0aaa0" || e.Params["Owner"] != "dev" {
		t.Fatalf("env = %+v", e)
	}
	ch := c.waitChange(e.ChangeID, stPreparing)
	if ch.Status != stProposed || ch.ProposedBy != "dev" || !strings.Contains(ch.Reason, "Checkout work") || !strings.Contains(ch.Reason, "Trying the checkout branch") {
		t.Fatalf("change = %+v", ch)
	}
	for i := 0; i < 5; i++ { // the engine must not approve it
		c.srv.envTick(context.Background(), time.Now())
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.env(e.ID); got.Status != envRequested || len(c.cli.stacks) != 1 { // only the empty review stack
		t.Errorf("an unapproved environment moved: %+v, stacks %v", got.Status, c.cli.stacks)
	}
	c.dev.expect(403, "POST", "/v1/cloud/changes/"+e.ChangeID+"/approve", map[string]any{}) // not an administrator
	c.admin2.expect(202, "POST", "/v1/cloud/changes/"+e.ChangeID+"/approve", map[string]any{})
	act := c.waitEnv(e.ID, envActive, envFailed)
	if act.Status != envActive || act.ActivatedAt == nil || act.ExpiresAt == nil || act.ExpiresAt.Sub(*act.ActivatedAt) != 8*time.Hour {
		t.Fatalf("active = %+v", act)
	}
	if c.cli.stacks[e.Stack] != "CREATE_COMPLETE" {
		t.Errorf("stack = %q", c.cli.stacks[e.Stack])
	}
	log := string(c.admin.expect(200, "GET", "/v1/audit?limit=100", nil))
	for _, want := range []string{"env.request", "env.activate", "waits for an administrator"} {
		if !strings.Contains(log, want) {
			t.Errorf("audit log has no %q", want)
		}
	}
	// The change carried the owner, expiry and network the server chose.
	if !strings.Contains(c.cli.paramsSeen, `"ParameterKey":"Owner","ParameterValue":"dev"`) || !strings.Contains(c.cli.paramsSeen, "ExpiresAt") {
		t.Errorf("parameters = %s", c.cli.paramsSeen)
	}
}

func TestEnvSelfServiceStartsWithoutAPersonAndIsAudited(t *testing.T) {
	c := envHarness(t)
	c.setPolicy(selfService("dev-box"))
	e := c.requestEnv(c.dev, 202, nil)
	if !e.AutoApprove {
		t.Fatalf("env = %+v", e)
	}
	act := c.waitEnv(e.ID, envActive, envFailed, envRejected)
	if act.Status != envActive {
		t.Fatalf("env = %+v", act)
	}
	ch := c.waitChange(e.ChangeID, stPreparing, stProposed, stExecuting)
	if ch.ProposedBy != "dev" || ch.DecidedBy != "policy:self-service" || ch.Status != stSucceeded {
		t.Errorf("change = %+v", ch)
	}
	if !strings.Contains(string(c.admin.expect(200, "GET", "/v1/audit?limit=100", nil)), "env.approve") {
		t.Error("the policy's approval is not in the audit log")
	}
	// A request outside the policy still waits, for the same person.
	big := c.requestEnv(c.dev, 202, map[string]any{"name": "Big box", "params": map[string]string{"InstanceType": "t4g.medium", "DiskGiB": "100"}})
	if big.AutoApprove {
		t.Fatal("an over-limit request was marked for self-service")
	}
	c.waitChange(big.ChangeID, stPreparing)
	for i := 0; i < 5; i++ {
		c.srv.envTick(context.Background(), time.Now())
	}
	if got := c.env(big.ID); got.Status != envRequested {
		t.Errorf("an over-limit request ran on its own: %s", got.Status)
	}
}

func TestEnvSelfServiceCannotBeSelfGranted(t *testing.T) {
	c := envHarness(t)
	var e Environment
	resp, body := c.dev.do("POST", "/v1/envs", map[string]any{"template": "dev-box", "name": "Sneaky", "purpose": "Trying to skip approval", "ttl_hours": 4, "auto_approve": true, "approved": true})
	if resp.StatusCode == 202 {
		json.Unmarshal(body, &e)
		if e.AutoApprove || e.Approved {
			t.Fatalf("a client set its own approval: %+v", e)
		}
		c.waitChange(e.ChangeID, stPreparing)
		for i := 0; i < 5; i++ {
			c.srv.envTick(context.Background(), time.Now())
		}
		if got := c.env(e.ID); got.Status != envRequested {
			t.Errorf("it ran without approval: %s", got.Status)
		}
	} // a 400 for unknown fields is as good
	// Only an administrator changes the policy.
	c.dev.expect(403, "PUT", "/v1/envs/policy", selfService("dev-box"))
	c.view.expect(403, "GET", "/v1/envs/policy", nil)
}

func TestEnvRequestValidation(t *testing.T) {
	c := envHarness(t)
	post := func(mut map[string]any) string {
		req := map[string]any{"template": "dev-box", "name": "Valid name", "purpose": "A real reason", "ttl_hours": 8}
		for k, v := range mut {
			req[k] = v
		}
		return string(c.dev.expect(400, "POST", "/v1/envs", req))
	}
	for name, tc := range map[string]struct {
		mut  map[string]any
		want string
	}{
		"bad name":         {map[string]any{"name": "x"}, "3 to 40"},
		"name starts a 1":  {map[string]any{"name": "1abc"}, "3 to 40"},
		"short purpose":    {map[string]any{"purpose": "no"}, "5 to 300"},
		"not an env":       {map[string]any{"template": "forgeline-demo"}, "environment templates"},
		"unknown template": {map[string]any{"template": "ghost"}, "environment templates"},
		"zero hours":       {map[string]any{"ttl_hours": 0}, "between 1 and"},
		"too many hours":   {map[string]any{"ttl_hours": 9999}, "between 1 and"},
		"owner override":   {map[string]any{"params": map[string]string{"Owner": "someone-else"}}, "not something you can set"},
		"network override": {map[string]any{"params": map[string]string{"VpcId": "vpc-0123456789abcdef0"}}, "not something you can set"},
		"bad disk":         {map[string]any{"params": map[string]string{"DiskGiB": "5000"}}, "not valid"},
		"bad type":         {map[string]any{"params": map[string]string{"InstanceType": "p4d.24xlarge"}}, "must be one of"},
	} {
		if msg := post(tc.mut); !strings.Contains(msg, tc.want) {
			t.Errorf("%s: %s", name, msg)
		}
	}
	c.view.expect(403, "POST", "/v1/envs", map[string]any{})
	c.cli.mu.Lock()
	c.cli.noVPC = true
	c.cli.mu.Unlock()
	if msg := c.dev.expect(409, "POST", "/v1/envs", map[string]any{"template": "dev-box", "name": "No network", "purpose": "A real reason", "ttl_hours": 4}); !strings.Contains(string(msg), "no default VPC") {
		t.Errorf("no default VPC: %s", msg)
	}
	// The QA sandbox needs no network, so a missing VPC does not matter.
	c.requestEnv(c.dev, 202, map[string]any{"template": "qa-sandbox", "name": "Queue test"})
}

func TestEnvLimitPerPerson(t *testing.T) {
	c := envHarness(t)
	p := selfService("dev-box")
	p.MaxActivePerUser = 1
	c.setPolicy(p)
	first := c.requestEnv(c.dev, 202, map[string]any{"name": "First one"})
	if msg := c.dev.expect(409, "POST", "/v1/envs", map[string]any{"template": "dev-box", "name": "Second one", "purpose": "A real reason", "ttl_hours": 4}); !strings.Contains(string(msg), "limit is 1") {
		t.Errorf("limit: %s", msg)
	}
	// Someone else is unaffected, and a deleted environment frees the slot.
	dev2 := c.h().login("dev2")
	c.requestEnv(dev2, 202, map[string]any{"name": "Other person"})
	c.waitEnv(first.ID, envActive)
	c.dev.expect(202, "POST", "/v1/envs/"+first.ID+"/destroy", map[string]any{})
	c.waitEnv(first.ID, envDestroyed)
	c.requestEnv(c.dev, 202, map[string]any{"name": "Second one"})
}

func TestEnvExpiresAndIsDeletedByPolicy(t *testing.T) {
	c := envHarness(t)
	c.setPolicy(selfService("dev-box"))
	e := c.requestEnv(c.dev, 202, nil)
	act := c.waitEnv(e.ID, envActive)
	// Not yet: the engine leaves it alone.
	for i := 0; i < 3; i++ {
		c.srv.envTick(context.Background(), time.Now())
	}
	if c.env(e.ID).Status != envActive {
		t.Fatal("deleted before its time")
	}
	// Time passes.
	late := act.ExpiresAt.Add(time.Minute)
	c.srv.envTick(context.Background(), late)
	if got := c.env(e.ID); got.Status != envDestroying || got.DestroyBy != "policy:expiry" {
		t.Fatalf("after expiry: %+v", got)
	}
	done := c.waitEnv(e.ID, envDestroyed, envFailed)
	if done.Status != envDestroyed || done.FinishedAt == nil {
		t.Fatalf("done = %+v", done)
	}
	if _, still := c.cli.stacks[e.Stack]; still {
		t.Errorf("the stack outlived its environment: %v", c.cli.stacks)
	}
	del := c.waitChange(done.DestroyChangeID, stPreparing, stProposed, stExecuting)
	if del.DecidedBy != "policy:expiry" || del.Kind != kindStackDelete || del.Stack != e.Stack {
		t.Errorf("delete change = %+v", del)
	}
	if !strings.Contains(string(c.admin.expect(200, "GET", "/v1/audit?limit=100", nil)), "env.expire") {
		t.Error("expiry is not audited")
	}
}

func TestEnvOwnerAndAdminCanDeleteOthersCannot(t *testing.T) {
	c := envHarness(t)
	c.setPolicy(selfService("dev-box"))
	e := c.requestEnv(c.dev, 202, nil)
	c.dev.expect(409, "POST", "/v1/envs/"+e.ID+"/destroy", map[string]any{}) // still being created
	c.waitEnv(e.ID, envActive)
	dev2 := c.h().login("dev2")
	dev2.expect(403, "POST", "/v1/envs/"+e.ID+"/destroy", map[string]any{})
	c.view.expect(403, "POST", "/v1/envs/"+e.ID+"/destroy", map[string]any{})
	c.admin.expect(202, "POST", "/v1/envs/"+e.ID+"/destroy", map[string]any{})
	got := c.waitEnv(e.ID, envDestroyed)
	if got.DestroyBy != "owner:ada" {
		t.Errorf("deleted by %q, want the administrator who asked", got.DestroyBy)
	}
	c.dev.expect(409, "POST", "/v1/envs/"+e.ID+"/destroy", map[string]any{}) // already gone
	c.dev.expect(404, "POST", "/v1/envs/ghost/destroy", map[string]any{})
}

func TestEnvExtend(t *testing.T) {
	c := envHarness(t)
	p := selfService("dev-box")
	p.MaxLifetimeHours = 24
	c.setPolicy(p)
	e := c.requestEnv(c.dev, 202, map[string]any{"ttl_hours": 8})
	act := c.waitEnv(e.ID, envActive)
	ext := func(b *browser, want, hours int) Environment {
		var out Environment
		json.Unmarshal(b.expect(want, "POST", "/v1/envs/"+e.ID+"/extend", map[string]any{"hours": hours}), &out)
		return out
	}
	got := ext(c.dev, 200, 4)
	if got.ExpiresAt.Sub(*act.ExpiresAt) != 4*time.Hour || got.Extensions != 1 {
		t.Errorf("extended = %+v", got)
	}
	dev2 := c.h().login("dev2")
	ext(dev2, 403, 1)
	ext(c.view, 403, 1)
	ext(c.dev, 400, 0)
	ext(c.dev, 400, maxExtendHrs+1)
	if msg := c.dev.expect(409, "POST", "/v1/envs/"+e.ID+"/extend", map[string]any{"hours": 24}); !strings.Contains(string(msg), "longest an environment may live") {
		t.Errorf("past the lifetime: %s", msg)
	}
	ext(c.dev, 200, 4)
	ext(c.dev, 200, 4)
	if msg := c.dev.expect(409, "POST", "/v1/envs/"+e.ID+"/extend", map[string]any{"hours": 1}); !strings.Contains(string(msg), "extended 3 times") {
		t.Errorf("third extension limit: %s", msg)
	}
	ext(c.admin, 409, 24) // administrators skip the count, not the lifetime
	ext(c.admin, 200, 1)
	c.dev.expect(409, "POST", "/v1/envs/"+c.requestEnv(c.dev, 202, map[string]any{"name": "Not yet"}).ID+"/extend", map[string]any{"hours": 1})
}

func TestEnvFailedCreationIsCleanedUp(t *testing.T) {
	c := envHarness(t)
	c.setPolicy(selfService("dev-box"))
	c.cli.mu.Lock()
	c.cli.failCreate = true
	c.cli.mu.Unlock()
	e := c.requestEnv(c.dev, 202, nil)
	done := c.waitEnv(e.ID, envFailed)
	for i := 0; i < 400 && c.cli.stacks[e.Stack] != ""; i++ { // cleanup runs behind the status
		c.srv.envTick(context.Background(), time.Now())
		time.Sleep(5 * time.Millisecond)
		done = c.env(e.ID)
	}
	if done.Status != envFailed || !strings.Contains(done.Note, "could not be created") || done.FinishedAt == nil {
		t.Fatalf("env = %+v", done)
	}
	if _, left := c.cli.stacks[e.Stack]; left {
		t.Errorf("a failed environment left its stack behind: %v", c.cli.stacks)
	}
	if done.DestroyBy != "policy:cleanup" {
		t.Errorf("cleanup was by %q", done.DestroyBy)
	}
	// It no longer counts against the person's limit.
	c.cli.mu.Lock()
	c.cli.failCreate = false
	c.cli.mu.Unlock()
	c.requestEnv(c.dev, 202, map[string]any{"name": "Try again"})
}

func TestEnvRejectedRequestCreatesNothing(t *testing.T) {
	c := envHarness(t)
	e := c.requestEnv(c.dev, 202, nil)
	c.waitChange(e.ChangeID, stPreparing)
	c.admin2.expect(200, "POST", "/v1/cloud/changes/"+e.ChangeID+"/reject", map[string]any{"note": "not now"})
	got := c.waitEnv(e.ID, envRejected)
	if !strings.Contains(got.Note, "nothing was created") || got.FinishedAt == nil {
		t.Errorf("env = %+v", got)
	}
	if c.cli.stacks[e.Stack] == "CREATE_COMPLETE" {
		t.Error("a rejected request created a stack")
	}
}

func TestEnvPolicyValidationAndPersistence(t *testing.T) {
	c := envHarness(t)
	var p EnvPolicy
	json.Unmarshal(c.dev.expect(200, "GET", "/v1/envs/policy", nil), &p)
	if p.SelfService || p.MaxTTLHours != 24 || p.MaxLifetimeHours != 168 || p.MaxActivePerUser != 2 || p.MaxMonthlyUSD != 40 || len(p.Templates) != 0 {
		t.Errorf("defaults = %+v", p)
	}
	good := selfService("dev-box", "qa-sandbox")
	for _, mut := range map[string]func(*EnvPolicy){
		"ttl 0":            func(p *EnvPolicy) { p.MaxTTLHours = 0 },
		"ttl too long":     func(p *EnvPolicy) { p.MaxTTLHours = 200 },
		"life under ttl":   func(p *EnvPolicy) { p.MaxLifetimeHours = 10 },
		"life too long":    func(p *EnvPolicy) { p.MaxLifetimeHours = 1000 },
		"per user 0":       func(p *EnvPolicy) { p.MaxActivePerUser = 0 },
		"per user 50":      func(p *EnvPolicy) { p.MaxActivePerUser = 50 },
		"cost 0":           func(p *EnvPolicy) { p.MaxMonthlyUSD = 0 },
		"cost 5000":        func(p *EnvPolicy) { p.MaxMonthlyUSD = 5000 },
		"unknown template": func(p *EnvPolicy) { p.Templates = []string{"forgeline-demo"} },
	} {
		bad := good
		mut(&bad)
		c.admin.expect(400, "PUT", "/v1/envs/policy", bad)
	}
	c.setPolicy(good)
	json.Unmarshal(c.dev.expect(200, "GET", "/v1/envs/policy", nil), &p)
	if !p.SelfService || len(p.Templates) != 2 || p.UpdatedBy != "ada" {
		t.Errorf("saved = %+v", p)
	}
	if !strings.Contains(string(c.admin.expect(200, "GET", "/v1/audit?limit=50", nil)), "env.policy") {
		t.Error("a policy change is not audited")
	}
}

func TestEnvWithoutAnAWSAccount(t *testing.T) {
	c := newCloudHarness(t) // not connected
	c.dev.expect(409, "POST", "/v1/envs", map[string]any{"template": "dev-box", "name": "Nowhere", "purpose": "A real reason", "ttl_hours": 4})
	c.dev.expect(409, "POST", "/v1/envs", map[string]any{"template": "qa-sandbox", "name": "Nowhere", "purpose": "A real reason", "ttl_hours": 4})
}

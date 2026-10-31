package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func workflowPkg(version int) MarketPackage {
	p := samplePkg(version, "Steps.")
	p.ID, p.Title = "flow-pack", "Flow pack"
	p.Workflows = []MarketWorkflow{{Key: "triage", Name: "Nightly triage", Description: "Looks at failed runs", RunAs: "developer", Stages: []WFStage{
		{Name: "Look", Steps: []WFStep{{Agent: "a", Task: "List failed runs from the last day."}}},
		{Name: "Report", Gate: true, Steps: []WFStep{{Agent: "a", Task: "Summarise what you found."}}},
	}}}
	return p
}

func TestWorkflowPackageChecks(t *testing.T) {
	p := workflowPkg(1)
	p.SHA256 = p.contentHash()
	if err := p.check(); err != nil {
		t.Fatalf("good package refused: %v", err)
	}
	bad := map[string]func(*MarketPackage){
		"agent not in the package": func(p *MarketPackage) { p.Workflows[0].Stages[0].Steps[0].Agent = "ghost" },
		"step id set":              func(p *MarketPackage) { p.Workflows[0].Stages[0].Steps[0].ID = "s1-1" },
		"agent version pinned":     func(p *MarketPackage) { p.Workflows[0].Stages[0].Steps[0].AgentN = 2 },
		"empty task":               func(p *MarketPackage) { p.Workflows[0].Stages[0].Steps[0].Task = " " },
		"huge task":                func(p *MarketPackage) { p.Workflows[0].Stages[0].Steps[0].Task = strings.Repeat("x", maxWFTask+1) },
		"admin run":                func(p *MarketPackage) { p.Workflows[0].RunAs = "admin" },
		"no stages":                func(p *MarketPackage) { p.Workflows[0].Stages = nil },
		"too many stages":          func(p *MarketPackage) { p.Workflows[0].Stages = make([]WFStage, maxWFStages+1) },
		"empty stage":              func(p *MarketPackage) { p.Workflows[0].Stages[0].Steps = nil },
		"repeated key":             func(p *MarketPackage) { p.Workflows = append(p.Workflows, p.Workflows[0]) },
		"no name":                  func(p *MarketPackage) { p.Workflows[0].Name = "" },
	}
	for name, mut := range bad {
		q := workflowPkg(1)
		mut(&q)
		q.SHA256 = q.contentHash()
		if q.check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The package form has no schedule field at all, so a schedule can't be smuggled in.
	raw, _ := json.Marshal(workflowPkg(1))
	if strings.Contains(string(raw), "every_minutes") {
		t.Error("the package form must not carry a schedule")
	}
}

func TestWorkflowPackageInstallsUpgradesAndUninstalls(t *testing.T) {
	m := newMCPHarness(t)
	publish(t, workflowPkg(1))
	e := listMarket(t, m.admin)[0]
	var in MarketInstall
	json.Unmarshal(m.admin.expect(201, "POST", "/v1/market/install", map[string]any{"id": "flow-pack", "hash": e.SHA256}), &in)
	id := in.Workflows["triage"]
	if id == "" || in.Agents["a"] == "" {
		t.Fatalf("install = %+v", in)
	}
	var wf Workflow
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/workflows/"+id, nil), &wf)
	v := wf.Versions[0]
	// At T0 the workflow is lowered to viewer, steps point at the agent this install made, the gate
	// is kept and there is no schedule.
	if v.RunAs != "viewer" || v.EveryMinutes != 0 || len(v.Stages) != 2 || !v.Stages[1].Gate || v.Stages[0].Steps[0].Agent != in.Agents["a"] || wf.Disabled {
		t.Errorf("workflow = %+v", v)
	}
	if !strings.Contains(v.Note, "") || wf.Name != "Nightly triage" {
		t.Errorf("workflow = %+v", wf)
	}
	// Upgrade: unchanged content adds nothing, changed content adds a version.
	p2 := workflowPkg(2)
	publish(t, p2)
	e = listMarket(t, m.admin)[0]
	m.admin.expect(201, "POST", "/v1/market/install", map[string]any{"id": "flow-pack", "hash": e.SHA256})
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/workflows/"+id, nil), &wf)
	if len(wf.Versions) != 1 {
		t.Errorf("unchanged workflow got %d versions", len(wf.Versions))
	}
	p3 := workflowPkg(3)
	p3.Workflows[0].Stages[0].Steps[0].Task = "List failed runs from the last two days."
	publish(t, p3)
	e = listMarket(t, m.admin)[0]
	m.admin.expect(201, "POST", "/v1/market/install", map[string]any{"id": "flow-pack", "hash": e.SHA256})
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/workflows/"+id, nil), &wf)
	if len(wf.Versions) != 2 || wf.Current != 2 || !strings.Contains(wf.Versions[1].Stages[0].Steps[0].Task, "two days") {
		t.Errorf("upgraded workflow = %+v", wf)
	}
	// Uninstall turns it off and keeps the history.
	m.admin.expect(200, "DELETE", "/v1/market/install/flow-pack", nil)
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/workflows/"+id, nil), &wf)
	if !wf.Disabled || len(wf.Versions) != 2 {
		t.Errorf("after uninstall = %+v", wf)
	}
}

func TestWorkflowPackageAtT1KeepsDeveloper(t *testing.T) {
	m := newMCPHarness(t)
	publish(t, workflowPkg(1))
	e := listMarket(t, m.admin)[0]
	var in MarketInstall
	json.Unmarshal(m.admin.expect(201, "POST", "/v1/market/install", map[string]any{"id": "flow-pack", "hash": e.SHA256, "tier": "T1"}), &in)
	var wf Workflow
	json.Unmarshal(m.admin.expect(200, "GET", "/v1/ai/workflows/"+in.Workflows["triage"], nil), &wf)
	if wf.Versions[0].RunAs != "developer" {
		t.Errorf("run_as = %s", wf.Versions[0].RunAs)
	}
}

const sandboxBody = `AWSTemplateFormatVersion: "2010-09-09"
Parameters:
  Owner: {Type: String}
  ExpiresAt: {Type: String}
  Flavor: {Type: String, Default: small}
Resources:
  Queue:
    Type: AWS::SQS::Queue
    Properties:
      KmsMasterKeyId: alias/aws/sqs
`

func envPkg(version int) MarketPackage {
	return MarketPackage{ID: "envs-pack", Version: version, Title: "Env pack", Description: "d", Publisher: "Acme",
		EnvTemplates: []MarketEnvTemplate{{Key: "queue", Title: "Test queue", Description: "An encrypted queue for a test run.", DefaultStack: "test-queue",
			DefaultTTLHours: 8, MonthlyUSD: 1.5, Body: sandboxBody, Parameters: []MarketParam{
				{Name: "Owner", Label: "Owner", Required: true, Pattern: `[A-Za-z0-9_.@ -]{1,64}`},
				{Name: "ExpiresAt", Label: "Expires at", Required: true, Pattern: `[0-9TZ:-]{20}`},
				{Name: "Flavor", Label: "Flavor", Default: "small", Options: []string{"small", "large"}},
			}}}}
}

func TestEnvTemplatePackageChecks(t *testing.T) {
	p := envPkg(1)
	p.SHA256 = p.contentHash()
	if err := p.check(); err != nil {
		t.Fatalf("good package refused: %v", err)
	}
	bad := map[string]func(*MarketPackage){
		"IAM role":        func(p *MarketPackage) { p.EnvTemplates[0].Body += "  R:\n    Type: AWS::IAM::Role\n" },
		"IAM lowercase":   func(p *MarketPackage) { p.EnvTemplates[0].Body += "  R:\n    Type: aws::iam::role\n" },
		"nested stack":    func(p *MarketPackage) { p.EnvTemplates[0].Body += "  S:\n    Type: AWS::CloudFormation::Stack\n" },
		"custom resource": func(p *MarketPackage) { p.EnvTemplates[0].Body += "  C:\n    Type: Custom::Thing\n" },
		"cfn custom": func(p *MarketPackage) {
			p.EnvTemplates[0].Body += "  C:\n    Type: AWS::CloudFormation::CustomResource\n"
		},
		"macro":      func(p *MarketPackage) { p.EnvTemplates[0].Body += "Transform: AWS::Include\n" },
		"serverless": func(p *MarketPackage) { p.EnvTemplates[0].Body += "  F:\n    Type: AWS::Serverless::Function\n" },
		"huge body": func(p *MarketPackage) {
			p.EnvTemplates[0].Body = sandboxBody + strings.Repeat("# x\n", maxTemplateBody)
		},
		"empty body":     func(p *MarketPackage) { p.EnvTemplates[0].Body = " " },
		"no Owner":       func(p *MarketPackage) { p.EnvTemplates[0].Parameters = p.EnvTemplates[0].Parameters[1:] },
		"Owner optional": func(p *MarketPackage) { p.EnvTemplates[0].Parameters[0].Required = false },
		"no ExpiresAt": func(p *MarketPackage) {
			p.EnvTemplates[0].Parameters = append(p.EnvTemplates[0].Parameters[:1], p.EnvTemplates[0].Parameters[2:]...)
		},
		"bad pattern":      func(p *MarketPackage) { p.EnvTemplates[0].Parameters[2].Pattern = `(` },
		"default off-list": func(p *MarketPackage) { p.EnvTemplates[0].Parameters[2].Default = "huge" },
		"repeated param": func(p *MarketPackage) {
			p.EnvTemplates[0].Parameters = append(p.EnvTemplates[0].Parameters, p.EnvTemplates[0].Parameters[2])
		},
		"bad param name": func(p *MarketPackage) { p.EnvTemplates[0].Parameters[2].Name = "bad name" },
		"no TTL":         func(p *MarketPackage) { p.EnvTemplates[0].DefaultTTLHours = 0 },
		"long TTL":       func(p *MarketPackage) { p.EnvTemplates[0].DefaultTTLHours = 9999 },
		"negative cost":  func(p *MarketPackage) { p.EnvTemplates[0].MonthlyUSD = -1 },
		"huge cost":      func(p *MarketPackage) { p.EnvTemplates[0].MonthlyUSD = 5000 },
		"bad stack":      func(p *MarketPackage) { p.EnvTemplates[0].DefaultStack = "1bad" },
		"bad key":        func(p *MarketPackage) { p.EnvTemplates[0].Key = "Bad Key" },
		"repeated key":   func(p *MarketPackage) { p.EnvTemplates = append(p.EnvTemplates, p.EnvTemplates[0]) },
		"too many params": func(p *MarketPackage) {
			p.EnvTemplates[0].Parameters = append(p.EnvTemplates[0].Parameters, make([]MarketParam, maxTplParams)...)
		},
	}
	for name, mut := range bad {
		q := envPkg(1)
		mut(&q)
		q.SHA256 = q.contentHash()
		if q.check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEnvTemplatePackageJoinsTheCatalogButNeverSelfService(t *testing.T) {
	c := envHarness(t)
	publish(t, envPkg(1))
	var pk struct{ Packages []marketEntry }
	json.Unmarshal(c.admin.expect(200, "GET", "/v1/market/packages", nil), &pk)
	e := pk.Packages[0]
	id := "pkg.envs-pack.queue"
	// Before install the template is not offered or requestable.
	c.dev.expect(400, "POST", "/v1/envs", map[string]any{"template": id, "name": "Queue test", "purpose": "Trying the queue", "ttl_hours": 4})
	var in MarketInstall
	json.Unmarshal(c.admin.expect(201, "POST", "/v1/market/install", map[string]any{"id": "envs-pack", "hash": e.SHA256}), &in)
	if len(in.Templates) != 1 || in.Templates[0] != id {
		t.Fatalf("install = %+v", in)
	}
	var tpls struct {
		Templates []struct {
			ID         string
			Kind       string
			MonthlyUSD float64 `json:"monthly_usd"`
			Params     []CatalogParam
		}
	}
	json.Unmarshal(c.dev.expect(200, "GET", "/v1/envs/templates", nil), &tpls)
	found := false
	for _, tp := range tpls.Templates {
		if tp.ID == id {
			found = true
			names := ""
			for _, p := range tp.Params {
				names += p.Name + ","
			}
			if tp.MonthlyUSD != 1.5 || names != "Flavor," || tp.Kind != "Packaged template" {
				t.Errorf("template = %+v", tp)
			}
		}
	}
	if !found {
		t.Fatalf("not in the template list: %+v", tpls)
	}
	// Self-service can never include it, even if an administrator tries.
	c.admin.expect(400, "PUT", "/v1/envs/policy", selfService(id))
	c.setPolicy(selfService("qa-sandbox"))
	var q envQuote
	json.Unmarshal(c.dev.expect(200, "POST", "/v1/envs/estimate", map[string]any{"template": id, "ttl_hours": 4}), &q)
	if q.SelfService || q.MonthlyUSD != 1.5 || !strings.Contains(q.Why, "not on the self-service list") {
		t.Errorf("quote = %+v", q)
	}
	// What the server decides can't be set by the requester.
	c.dev.expect(400, "POST", "/v1/envs", map[string]any{"template": id, "name": "Queue test", "purpose": "Trying the queue", "ttl_hours": 4, "params": map[string]string{"Owner": "someone else"}})
	c.dev.expect(400, "POST", "/v1/envs/estimate", map[string]any{"template": id, "ttl_hours": 4, "params": map[string]string{"Flavor": "huge"}})
	// A request waits for another administrator, and its change set carries no capabilities.
	env := c.requestEnv(c.dev, 202, map[string]any{"template": id, "params": map[string]string{"Flavor": "large"}})
	if env.Status != envRequested || env.AutoApprove {
		t.Errorf("env = %+v", env)
	}
	tpl, ok := c.srv.findTemplate(context.Background(), id)
	if !ok || len(tpl.capabilities) != 0 || tpl.body != sandboxBody {
		t.Errorf("template = %+v", tpl)
	}
	var cat struct{ Templates []CatalogTemplate }
	json.Unmarshal(c.dev.expect(200, "GET", "/v1/cloud/catalog", nil), &cat)
	if len(cat.Templates) < 4 {
		t.Errorf("the cloud catalog should list the packaged template too: %d", len(cat.Templates))
	}
	// Uninstall removes it for new requests.
	c.admin.expect(200, "DELETE", "/v1/market/install/envs-pack", nil)
	c.dev.expect(400, "POST", "/v1/envs", map[string]any{"template": id, "name": "Queue again", "purpose": "Trying the queue", "ttl_hours": 4})
	if _, ok := c.srv.findTemplate(context.Background(), id); ok {
		t.Error("an uninstalled template can still be found")
	}
}

func TestEnvTemplatePackageUpgradeReplacesAndDropsTemplates(t *testing.T) {
	c := envHarness(t)
	p1 := envPkg(1)
	p1.EnvTemplates = append(p1.EnvTemplates, p1.EnvTemplates[0])
	p1.EnvTemplates[1].Key = "second"
	publish(t, p1)
	var pk struct{ Packages []marketEntry }
	json.Unmarshal(c.admin.expect(200, "GET", "/v1/market/packages", nil), &pk)
	c.admin.expect(201, "POST", "/v1/market/install", map[string]any{"id": "envs-pack", "hash": pk.Packages[0].SHA256})
	if _, ok := c.srv.findTemplate(context.Background(), "pkg.envs-pack.second"); !ok {
		t.Fatal("second template missing after install")
	}
	p2 := envPkg(2)
	p2.EnvTemplates[0].MonthlyUSD = 9
	publish(t, p2)
	json.Unmarshal(c.admin.expect(200, "GET", "/v1/market/packages", nil), &pk)
	c.admin.expect(201, "POST", "/v1/market/install", map[string]any{"id": "envs-pack", "hash": pk.Packages[0].SHA256})
	if _, ok := c.srv.findTemplate(context.Background(), "pkg.envs-pack.second"); ok {
		t.Error("a template removed by the new version is still offered")
	}
	spec, ok := c.srv.envSpecFor(context.Background(), "pkg.envs-pack.queue")
	if !ok || spec.Monthly(nil) != 9 {
		t.Errorf("upgraded spec = %+v", spec)
	}
}

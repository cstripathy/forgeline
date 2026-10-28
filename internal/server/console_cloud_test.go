package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	awsdeploy "github.com/forgelinehq/forgeline/deploy/aws"
	"github.com/forgelinehq/forgeline/internal/authn"
	"github.com/forgelinehq/forgeline/internal/executor"
)

// fakeCLI stands in for the docker and aws command lines.
type fakeCLI struct {
	mu      sync.Mutex
	account string
	// loggedIn false makes every aws call fail like an expired SSO session.
	loggedIn bool
	deny     map[string]bool // "cloudformation describe-stacks" -> AccessDenied

	stacks     map[string]string // name -> status
	stackSeen  map[string]int    // describe-stacks calls since execute, to simulate progress
	changeSets map[string]string // "stack/name" -> status
	noChanges  bool
	instances  map[string]string // id -> state
	calls      []string
	paramsSeen string
	tagsSeen   []string
	costCalls  int
	dockerErr  string
}

func newFakeCLI() *fakeCLI {
	return &fakeCLI{account: "111122223333", loggedIn: true, deny: map[string]bool{},
		stacks: map[string]string{}, stackSeen: map[string]int{}, changeSets: map[string]string{},
		instances: map[string]string{"i-0aaa1111bbbb2222c": "stopped", "i-0ddd3333eeee4444f": "running"}}
}

func (f *fakeCLI) called(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func arg(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (f *fakeCLI) run(_ context.Context, name string, args ...string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name == "docker" {
		return f.docker(args)
	}
	// Drop the global flags the server adds.
	var a []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--profile", "--region", "--output":
			i++
		default:
			a = append(a, args[i])
		}
	}
	key := strings.Join(a[:min(2, len(a))], " ")
	f.calls = append(f.calls, strings.Join(a, " "))
	if !f.loggedIn {
		return nil, "Error loading SSO Token: Token for dev does not exist", errors.New("exit 255")
	}
	if f.deny[key] {
		return nil, "An error occurred (AccessDenied) when calling the " + key + " operation: not authorized", errors.New("exit 254")
	}
	js := func(v any) ([]byte, string, error) { b, _ := json.Marshal(v); return b, "", nil }
	switch key {
	case "sts get-caller-identity":
		return js(map[string]string{"Account": f.account, "Arn": "arn:aws:sts::" + f.account + ":assumed-role/Admin/ada", "UserId": "AROA:ada"})
	case "cloudformation describe-stacks":
		if stack := arg(a, "--stack-name"); stack != "" {
			status, ok := f.stacks[stack]
			if !ok {
				return nil, "An error occurred (ValidationError) when calling the DescribeStacks operation: Stack with id " + stack + " does not exist", errors.New("exit 254")
			}
			// A stack being created or deleted finishes after a couple of polls.
			if strings.HasSuffix(status, "_IN_PROGRESS") && status != "REVIEW_IN_PROGRESS" {
				f.stackSeen[stack]++
				if f.stackSeen[stack] >= 2 {
					switch status {
					case "CREATE_IN_PROGRESS":
						f.stacks[stack] = "CREATE_COMPLETE"
					case "UPDATE_IN_PROGRESS":
						f.stacks[stack] = "UPDATE_COMPLETE"
					case "DELETE_IN_PROGRESS":
						delete(f.stacks, stack)
						return nil, "An error occurred (ValidationError): Stack with id " + stack + " does not exist", errors.New("exit 254")
					}
					status = f.stacks[stack]
				}
			}
			return js(map[string]any{"Stacks": []map[string]any{{"StackName": stack, "StackStatus": status}}})
		}
		var list []map[string]any
		for n, s := range f.stacks {
			list = append(list, map[string]any{"StackName": n, "StackStatus": s, "CreationTime": "2026-10-21T19:23:12Z",
				"Outputs": []map[string]string{{"OutputKey": "InstanceId", "OutputValue": "i-0aaa1111bbbb2222c"}}})
		}
		return js(map[string]any{"Stacks": list})
	case "cloudformation create-change-set":
		stack, cs := arg(a, "--stack-name"), arg(a, "--change-set-name")
		b, _ := os.ReadFile(strings.TrimPrefix(arg(a, "--parameters"), "file://"))
		f.paramsSeen = string(b)
		tpl, _ := os.ReadFile(strings.TrimPrefix(arg(a, "--template-body"), "file://"))
		if string(tpl) != awsdeploy.DemoTemplate {
			return nil, "template differs", errors.New("exit 1")
		}
		for i, x := range a {
			if x == "--tags" {
				f.tagsSeen = a[i+1 : i+3]
			}
		}
		if arg(a, "--change-set-type") == "CREATE" {
			f.stacks[stack] = "REVIEW_IN_PROGRESS"
		}
		f.changeSets[stack+"/"+cs] = "CREATE_COMPLETE"
		if f.noChanges {
			f.changeSets[stack+"/"+cs] = "FAILED"
		}
		return js(map[string]string{"Id": "arn:cs"})
	case "cloudformation describe-change-set":
		st, ok := f.changeSets[arg(a, "--stack-name")+"/"+arg(a, "--change-set-name")]
		if !ok {
			return nil, "ChangeSet does not exist", errors.New("exit 254")
		}
		out := map[string]any{"Status": st}
		if st == "FAILED" {
			out["StatusReason"] = "The submitted information didn't contain changes."
		} else {
			out["Changes"] = []map[string]any{
				{"ResourceChange": map[string]any{"Action": "Add", "LogicalResourceId": "Instance", "ResourceType": "AWS::EC2::Instance", "Replacement": "", "Scope": []string{"Properties"}}},
				{"ResourceChange": map[string]any{"Action": "Add", "LogicalResourceId": "InstanceRole", "ResourceType": "AWS::IAM::Role"}},
			}
		}
		return js(out)
	case "cloudformation delete-change-set":
		delete(f.changeSets, arg(a, "--stack-name")+"/"+arg(a, "--change-set-name"))
		return js(map[string]string{})
	case "cloudformation execute-change-set":
		stack := arg(a, "--stack-name")
		if f.stacks[stack] == "REVIEW_IN_PROGRESS" {
			f.stacks[stack] = "CREATE_IN_PROGRESS"
		} else {
			f.stacks[stack] = "UPDATE_IN_PROGRESS"
		}
		f.stackSeen[stack] = 0
		return js(map[string]string{})
	case "cloudformation delete-stack":
		stack := arg(a, "--stack-name")
		if f.stacks[stack] == "REVIEW_IN_PROGRESS" {
			delete(f.stacks, stack)
		} else {
			f.stacks[stack] = "DELETE_IN_PROGRESS"
			f.stackSeen[stack] = 0
		}
		return js(map[string]string{})
	case "cloudformation list-stack-resources":
		return js(map[string]any{"StackResourceSummaries": []map[string]string{
			{"LogicalResourceId": "Instance", "ResourceType": "AWS::EC2::Instance", "PhysicalResourceId": "i-0aaa1111bbbb2222c"}}})
	case "ec2 describe-instances":
		var res []map[string]any
		for id, state := range f.instances {
			if want := arg(a, "--instance-ids"); want != "" && want != id {
				continue
			}
			res = append(res, map[string]any{"Instances": []map[string]any{{
				"InstanceId": id, "InstanceType": "t4g.small", "Architecture": "arm64", "PrivateIpAddress": "172.31.0.5",
				"State": map[string]string{"Name": state},
				"Tags":  []map[string]string{{"Key": "Name", "Value": "forgeline-demo"}, {"Key": "aws:cloudformation:stack-name", "Value": "forgeline-demo"}},
			}}})
		}
		if len(res) == 0 {
			return nil, "InvalidInstanceID.NotFound", errors.New("exit 254")
		}
		return js(map[string]any{"Reservations": res})
	case "ec2 start-instances":
		f.instances[arg(a, "--instance-ids")] = "running"
		return js(map[string]string{})
	case "ec2 stop-instances":
		f.instances[arg(a, "--instance-ids")] = "stopped"
		return js(map[string]string{})
	case "ec2 reboot-instances":
		return js(map[string]string{})
	case "ce get-cost-and-usage":
		f.costCalls++
		return js(map[string]any{"ResultsByTime": []map[string]any{{"Groups": []map[string]any{
			{"Keys": []string{"Amazon Elastic Compute Cloud - Compute"}, "Metrics": map[string]any{"UnblendedCost": map[string]string{"Amount": "9.5", "Unit": "USD"}}},
			{"Keys": []string{"Tax"}, "Metrics": map[string]any{"UnblendedCost": map[string]string{"Amount": "1.5", "Unit": "USD"}}},
			{"Keys": []string{"Tiny"}, "Metrics": map[string]any{"UnblendedCost": map[string]string{"Amount": "0.001", "Unit": "USD"}}},
		}}}})
	}
	return nil, "unexpected call: " + key, errors.New("exit 1")
}

func (f *fakeCLI) docker(args []string) ([]byte, string, error) {
	if f.dockerErr != "" {
		return nil, f.dockerErr, errors.New("exit 1")
	}
	switch args[0] {
	case "info":
		return []byte(`{"ServerVersion":"29.3.0","Name":"devbox","OperatingSystem":"Ubuntu 24.04","NCPU":8,"MemTotal":16000000000,"Driver":"overlayfs","Containers":5,"ContainersRunning":1,"Images":9}`), "", nil
	case "ps":
		return []byte(`{"Names":"forgeline-abc123def456","Image":"alpine:3.24","Status":"Up 3 seconds","RunningFor":"3 seconds ago"}` + "\n" +
			`{"Names":"forgeline-testpg","Image":"postgres:18","Status":"Up","RunningFor":"1 hour ago"}`), "", nil
	}
	return nil, "unexpected docker call", errors.New("exit 1")
}

// cloudHarness is a server with a fake CLI, an admin, a developer and a viewer.
type cloudHarness struct {
	*harness
	cli                      *fakeCLI
	admin, admin2, dev, view *browser
}

func newCloudHarness(t *testing.T) *cloudHarness {
	t.Helper()
	h := newHarness(t, &executor.Fake{})
	cli := newFakeCLI()
	h.srv.run = cli.run
	h.srv.cloud.setInterval(5 * time.Millisecond)
	admin := h.adminBrowser("ada")
	admin.createUser("grace", authn.Admin)
	admin.createUser("dev", authn.Developer)
	admin.createUser("view", authn.Viewer)
	return &cloudHarness{harness: h, cli: cli, admin: admin, admin2: h.login("grace"), dev: h.login("dev"), view: h.login("view")}
}

func (c *cloudHarness) connectAWS() {
	c.t.Helper()
	c.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})
}

func (c *cloudHarness) waitChange(id string, notIn ...string) CloudChange {
	c.t.Helper()
	var ch CloudChange
	c.waitFor("change "+id+" to settle", func() bool {
		_, b := c.admin.do("GET", "/v1/cloud/changes/"+id, nil)
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

var goodDeploy = map[string]any{
	"kind": "stack-deploy", "template": "forgeline-demo", "reason": "Trying the demo in a fresh account",
	"params": map[string]string{"GitRef": strings.Repeat("a", 40), "VpcId": "vpc-0123456789abcdef0", "SubnetId": "subnet-0123456789abcdef0"},
}

func (c *cloudHarness) proposeDeploy(b *browser, mutate func(map[string]any)) CloudChange {
	c.t.Helper()
	req := map[string]any{}
	for k, v := range goodDeploy {
		req[k] = v
	}
	if mutate != nil {
		mutate(req)
	}
	var ch CloudChange
	json.Unmarshal(b.expect(202, "POST", "/v1/cloud/changes", req), &ch)
	return ch
}

func TestRuntimeOverviewShowsServerDockerAndStepContainers(t *testing.T) {
	c := newCloudHarness(t)
	var rt RuntimeJSON
	json.Unmarshal(c.view.expect(200, "GET", "/v1/infra/runtime", nil), &rt)
	if !rt.Docker.Available || rt.Docker.Version != "29.3.0" || rt.Docker.CPUs != 8 || rt.Docker.ContainersRunning != 1 {
		t.Errorf("docker = %+v", rt.Docker)
	}
	if len(rt.Steps) != 1 || rt.Steps[0].Name != "forgeline-abc123def456" {
		t.Errorf("steps = %+v", rt.Steps)
	}
	if rt.Server.Version == "" || rt.Server.Go == "" || rt.Server.MaxRuns < 1 || rt.Server.UptimeSeconds < 0 || rt.Server.DiskTotalBytes == 0 {
		t.Errorf("server = %+v", rt.Server)
	}
	if rt.Security.Users != 4 || rt.Security.Admins != 2 {
		t.Errorf("security = %+v", rt.Security)
	}

	c.cli.dockerErr = "Cannot connect to the Docker daemon at unix:///var/run/docker.sock"
	json.Unmarshal(c.view.expect(200, "GET", "/v1/infra/runtime", nil), &rt)
	if rt.Docker.Available || !strings.Contains(rt.Docker.Error, "Cannot connect") || len(rt.Steps) != 0 {
		t.Errorf("with docker down: %+v steps=%v", rt.Docker, rt.Steps)
	}
}

func TestAWSConnectionIsTestedBeforeItIsSaved(t *testing.T) {
	c := newCloudHarness(t)
	c.dev.expect(403, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})
	c.admin.expect(400, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "bad profile;rm", "region": "ap-south-1"})
	c.admin.expect(400, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "mars"})
	c.admin.expect(400, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev"})

	c.cli.loggedIn = false
	var res AWSSettingsResult
	json.Unmarshal(c.admin.expect(422, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"}), &res)
	if !strings.Contains(res.Error, "aws sso login --profile dev") {
		t.Errorf("error = %q, want the command to run", res.Error)
	}
	var cur AWSSettingsResult
	json.Unmarshal(c.dev.expect(200, "GET", "/v1/integrations/aws", nil), &cur)
	if cur.Enabled {
		t.Fatal("a connection that failed its test was saved")
	}

	c.cli.loggedIn = true
	json.Unmarshal(c.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"}), &res)
	if res.Identity == nil || res.Identity.Account != "111122223333" {
		t.Fatalf("result = %+v", res)
	}
}

func TestAWSViewListsStacksAndInstancesAndReportsPartialFailure(t *testing.T) {
	c := newCloudHarness(t)
	var v AWSJSON
	json.Unmarshal(c.view.expect(200, "GET", "/v1/infra/aws", nil), &v)
	if v.Enabled {
		t.Fatal("AWS should be off until connected")
	}
	c.connectAWS()
	c.cli.stacks["forgeline-demo"] = "CREATE_COMPLETE"
	v = AWSJSON{}
	json.Unmarshal(c.view.expect(200, "GET", "/v1/infra/aws?refresh=1", nil), &v)
	if v.Identity == nil || v.Identity.Account != "111122223333" || len(v.Stacks) != 1 || len(v.Instances) != 2 {
		t.Fatalf("view = %+v", v)
	}
	if v.Stacks[0].Outputs["InstanceId"] == "" || v.Instances[0].Stack != "forgeline-demo" || v.Instances[0].Name != "forgeline-demo" {
		t.Errorf("details missing: %+v %+v", v.Stacks[0], v.Instances[0])
	}

	// Caching: a second read within the TTL does not call AWS again.
	before := c.cli.called("sts")
	c.view.expect(200, "GET", "/v1/infra/aws", nil)
	if c.cli.called("sts") != before {
		t.Error("a cached read called AWS again")
	}

	// One section failing (no permission) must not hide the rest.
	c.cli.deny["cloudformation describe-stacks"] = true
	v = AWSJSON{}
	json.Unmarshal(c.view.expect(200, "GET", "/v1/infra/aws?refresh=1", nil), &v)
	if len(v.Instances) != 2 || !strings.Contains(v.SectionErrors["stacks"], "AccessDenied") && !strings.Contains(v.SectionErrors["stacks"], "denied") {
		t.Errorf("partial failure: instances=%d errors=%v", len(v.Instances), v.SectionErrors)
	}

	// An expired login is one clear message.
	c.cli.loggedIn = false
	v = AWSJSON{}
	json.Unmarshal(c.view.expect(200, "GET", "/v1/infra/aws?refresh=1", nil), &v)
	if !strings.Contains(v.Error, "aws sso login --profile dev") || v.Identity != nil {
		t.Errorf("expired login: %+v", v)
	}
}

func TestCostIsFetchedOnRequestAndCached(t *testing.T) {
	c := newCloudHarness(t)
	c.view.expect(403, "GET", "/v1/infra/aws/cost", nil)
	c.dev.expect(409, "GET", "/v1/infra/aws/cost", nil) // not connected
	c.connectAWS()
	var cost CostJSON
	json.Unmarshal(c.dev.expect(200, "GET", "/v1/infra/aws/cost", nil), &cost)
	if !cost.Available || cost.Total < 10.9 || cost.Total > 11.1 || cost.Unit != "USD" || len(cost.Services) != 2 || cost.Services[0].Service != "Amazon Elastic Compute Cloud - Compute" {
		t.Fatalf("cost = %+v", cost)
	}
	c.dev.expect(200, "GET", "/v1/infra/aws/cost", nil)
	if c.cli.costCalls != 1 {
		t.Errorf("Cost Explorer was called %d times; each call is billed, so it must be cached", c.cli.costCalls)
	}
	c.dev.expect(200, "GET", "/v1/infra/aws/cost?refresh=1", nil)
	if c.cli.costCalls != 2 {
		t.Errorf("an explicit refresh should call again, calls=%d", c.cli.costCalls)
	}
	c.cli.deny["ce get-cost-and-usage"] = true
	json.Unmarshal(c.dev.expect(200, "GET", "/v1/infra/aws/cost?refresh=1", nil), &cost)
	if cost.Available || cost.Error == "" {
		t.Errorf("denied cost: %+v", cost)
	}
}

func TestDeployFlowPreviewFourEyesAndExecution(t *testing.T) {
	c := newCloudHarness(t)
	c.dev.expect(409, "POST", "/v1/cloud/changes", goodDeploy) // AWS not connected yet
	c.connectAWS()

	c.view.expect(403, "POST", "/v1/cloud/changes", goodDeploy)
	ch := c.proposeDeploy(c.dev, nil)
	if ch.ID == "" || ch.Account != "111122223333" || ch.ProposedBy != "dev" {
		t.Fatalf("proposal = %+v", ch)
	}
	ch = c.waitChange(ch.ID, stPreparing)
	if ch.Status != stProposed || len(ch.Preview) != 2 || ch.Preview[0].Action != "Add" || ch.ChangeSetType != "CREATE" {
		t.Fatalf("prepared = %+v", ch)
	}
	// What was sent to AWS: validated parameters, managed tags, the exact embedded template.
	var params []map[string]string
	json.Unmarshal([]byte(c.cli.paramsSeen), &params)
	got := map[string]string{}
	for _, p := range params {
		got[p["ParameterKey"]] = p["ParameterValue"]
	}
	if got["GitRef"] != strings.Repeat("a", 40) || got["InstanceType"] != "t4g.small" || got["GoVersion"] != "1.27.3" {
		t.Errorf("parameters sent = %v", got)
	}
	if len(c.cli.tagsSeen) != 2 || c.cli.tagsSeen[0] != "Key=forgeline:managed,Value=true" {
		t.Errorf("tags = %v", c.cli.tagsSeen)
	}
	if n := c.cli.called("cloudformation execute-change-set"); n != 0 {
		t.Fatal("proposing executed the change")
	}

	// The proposer cannot approve; a developer cannot approve; both are audited as refusals.
	c.dev.expect(403, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]string{})
	c.admin.expect(202, "POST", "/v1/cloud/changes", mutated(goodDeploy, "stack", "second-stack"))
	if n := c.cli.called("cloudformation execute-change-set"); n != 0 {
		t.Fatal("a refused approval executed something")
	}

	// A different admin approves; it executes in the background and ends succeeded.
	c.admin2.expect(202, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]string{"note": "looks right"})
	ch = c.waitChange(ch.ID, stProposed, stExecuting)
	if ch.Status != stSucceeded || !strings.Contains(ch.Result, "CREATE_COMPLETE") || ch.DecidedBy != "grace" || ch.FinishedAt == nil {
		t.Fatalf("finished = %+v", ch)
	}
	c.admin2.expect(409, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]string{}) // not twice

	var audit struct{ Events []AuditEvent }
	json.Unmarshal(c.admin.expect(200, "GET", "/v1/audit?action=cloud", nil), &audit)
	byAction := map[string][]AuditEvent{}
	for _, e := range audit.Events {
		byAction[e.Action] = append(byAction[e.Action], e)
	}
	if len(byAction["cloud.propose"]) < 2 || len(byAction["cloud.execute"]) != 1 || byAction["cloud.execute"][0].Actor != "grace" {
		t.Errorf("audit = %+v", byAction)
	}
	refused := 0
	for _, e := range byAction["cloud.approve"] {
		if !e.Allowed {
			refused++
		}
	}
	if refused < 1 { // the second approval of an already finished change
		t.Errorf("refused approvals recorded = %d, want at least 1", refused)
	}
	var denied struct{ Events []AuditEvent }
	json.Unmarshal(c.admin.expect(200, "GET", "/v1/audit?action=access.denied", nil), &denied)
	devDenied := false
	for _, e := range denied.Events {
		devDenied = devDenied || (e.Actor == "dev" && strings.Contains(e.Target, "/approve"))
	}
	if !devDenied {
		t.Errorf("a developer trying to approve was not recorded: %+v", denied.Events)
	}
}

func mutated(base map[string]any, k string, v any) map[string]any {
	m := map[string]any{}
	for kk, vv := range base {
		m[kk] = vv
	}
	m[k] = v
	return m
}

func TestApproverCannotBeTheProposer(t *testing.T) {
	c := newCloudHarness(t)
	c.connectAWS()
	ch := c.proposeDeploy(c.admin, nil)
	c.waitChange(ch.ID, stPreparing)
	resp, body := c.admin.do("POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]string{})
	if resp.StatusCode != 403 || !strings.Contains(string(body), "someone else") {
		t.Fatalf("self approval = %d %s", resp.StatusCode, body)
	}
	// The shared bearer token is one actor too.
	tok := c.h().tokenPropose(mutated(goodDeploy, "stack", "token-stack"))
	c.waitChange(tok, stPreparing)
	resp, _ = c.h().do("POST", "/v1/cloud/changes/"+tok+"/approve", testToken, strings.NewReader(`{}`), "application/json")
	if resp.StatusCode != 403 {
		t.Fatalf("token approving its own proposal = %d, want 403", resp.StatusCode)
	}
}

func (c *cloudHarness) h() *cloudHarness { return c }

func (c *cloudHarness) tokenPropose(req map[string]any) string {
	b, _ := json.Marshal(req)
	resp, body := c.harness.do("POST", "/v1/cloud/changes", testToken, strings.NewReader(string(b)), "application/json")
	if resp.StatusCode != 202 {
		c.t.Fatalf("token propose = %d %s", resp.StatusCode, body)
	}
	var ch CloudChange
	json.Unmarshal(body, &ch)
	return ch.ID
}

func TestProposalValidation(t *testing.T) {
	c := newCloudHarness(t)
	c.connectAWS()
	bad := map[string]map[string]any{
		"no reason":       mutated(goodDeploy, "reason", "x"),
		"unknown kind":    mutated(goodDeploy, "kind", "format-disk"),
		"unknown tpl":     mutated(goodDeploy, "template", "evil"),
		"bad stack name":  mutated(goodDeploy, "stack", "bad name; rm"),
		"bad git ref":     mutated(goodDeploy, "params", map[string]string{"GitRef": "main", "VpcId": "vpc-0123456789abcdef0", "SubnetId": "subnet-0123456789abcdef0"}),
		"missing vpc":     mutated(goodDeploy, "params", map[string]string{"GitRef": strings.Repeat("a", 40), "SubnetId": "subnet-0123456789abcdef0"}),
		"unknown param":   mutated(goodDeploy, "params", map[string]string{"GitRef": strings.Repeat("a", 40), "VpcId": "vpc-0123456789abcdef0", "SubnetId": "subnet-0123456789abcdef0", "Evil": "1"}),
		"bad instance":    mutated(goodDeploy, "params", map[string]string{"GitRef": strings.Repeat("a", 40), "VpcId": "vpc-0123456789abcdef0", "SubnetId": "subnet-0123456789abcdef0", "InstanceType": "p4d.24xlarge"}),
		"delete no stack": {"kind": "stack-delete", "reason": "cleaning up the demo"},
		"bad instance id": {"kind": "instance-action", "instance_id": "i-nope", "action": "stop", "reason": "cost saving"},
		"bad action":      {"kind": "instance-action", "instance_id": "i-0aaa1111bbbb2222c", "action": "terminate", "reason": "cost saving"},
	}
	for name, req := range bad {
		resp, body := c.dev.do("POST", "/v1/cloud/changes", req)
		if resp.StatusCode != 400 {
			t.Errorf("%s: got %d %s, want 400", name, resp.StatusCode, body)
		}
	}
	c.dev.expect(403, "POST", "/v1/cloud/changes", map[string]any{"kind": "stack-delete", "stack": "CDKToolkit", "reason": "cleaning up everything"})
	c.dev.expect(403, "POST", "/v1/cloud/changes", map[string]any{"kind": "stack-delete", "stack": "cdktoolkit-x", "reason": "cleaning up everything"})
	if c.cli.called("cloudformation create-change-set") != 0 {
		t.Error("an invalid proposal reached AWS")
	}
}

func TestNoChangeAndStuckStackProposalsFailWithAReason(t *testing.T) {
	c := newCloudHarness(t)
	c.connectAWS()
	c.cli.stacks["forgeline-demo"] = "CREATE_COMPLETE"
	c.cli.noChanges = true
	ch := c.waitChange(c.proposeDeploy(c.dev, nil).ID, stPreparing)
	if ch.Status != stFailed || !strings.Contains(ch.Result, "nothing to change") || ch.ChangeSetType != "UPDATE" {
		t.Fatalf("no-op proposal = %+v", ch)
	}
	if c.cli.called("cloudformation delete-change-set") != 1 {
		t.Error("the empty change set was not cleaned up")
	}

	c.cli.noChanges = false
	c.cli.stacks["forgeline-demo"] = "ROLLBACK_COMPLETE"
	ch = c.waitChange(c.proposeDeploy(c.dev, nil).ID, stPreparing)
	if ch.Status != stFailed || !strings.Contains(ch.Result, "ROLLBACK_COMPLETE") {
		t.Fatalf("stuck stack proposal = %+v", ch)
	}
}

func TestRejectAndCancelCleanUpAndStopApproval(t *testing.T) {
	c := newCloudHarness(t)
	c.connectAWS()

	ch := c.waitChange(c.proposeDeploy(c.dev, nil).ID, stPreparing)
	if c.cli.stacks["forgeline-demo"] != "REVIEW_IN_PROGRESS" {
		t.Fatal("test setup: the review-only stack should exist")
	}
	c.dev.expect(403, "POST", "/v1/cloud/changes/"+ch.ID+"/reject", map[string]string{})
	c.admin.expect(200, "POST", "/v1/cloud/changes/"+ch.ID+"/reject", map[string]string{"note": "wrong commit"})
	if _, still := c.cli.stacks["forgeline-demo"]; still {
		t.Error("rejecting left a review-only stack behind")
	}
	c.admin2.expect(409, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]string{})
	got := c.waitChange(ch.ID)
	if got.Status != stRejected || got.DecidedBy != "ada" || got.DecisionNote != "wrong commit" {
		t.Errorf("rejected = %+v", got)
	}

	// The proposer can withdraw their own; another developer cannot.
	other := c.h()
	ch2 := c.waitChange(c.proposeDeploy(c.dev, func(m map[string]any) { m["stack"] = "other" }).ID, stPreparing)
	c.admin.createUser("dev2", authn.Developer)
	dev2 := other.login("dev2")
	dev2.expect(403, "POST", "/v1/cloud/changes/"+ch2.ID+"/cancel", map[string]string{})
	c.dev.expect(200, "POST", "/v1/cloud/changes/"+ch2.ID+"/cancel", map[string]string{})
	c.admin2.expect(409, "POST", "/v1/cloud/changes/"+ch2.ID+"/approve", map[string]string{})
}

func TestApprovalIsBoundToTheAccountItWasPreparedFor(t *testing.T) {
	c := newCloudHarness(t)
	c.connectAWS()
	ch := c.waitChange(c.proposeDeploy(c.dev, nil).ID, stPreparing)
	c.cli.account = "999988887777" // the server's credentials now point somewhere else
	resp, body := c.admin2.do("POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]string{})
	if resp.StatusCode != 409 || !strings.Contains(string(body), "999988887777") {
		t.Fatalf("approval in another account = %d %s", resp.StatusCode, body)
	}
	if c.cli.called("cloudformation execute-change-set") != 0 {
		t.Fatal("executed in the wrong account")
	}

	// The same for a changed connection (another profile or region).
	c.cli.account = "111122223333"
	c.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "us-east-1"})
	resp, body = c.admin2.do("POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]string{})
	if resp.StatusCode != 409 || !strings.Contains(string(body), "connection changed") {
		t.Fatalf("approval after the connection changed = %d %s", resp.StatusCode, body)
	}
}

func TestStaleProposalsExpire(t *testing.T) {
	c := newCloudHarness(t)
	c.connectAWS()
	ch := c.waitChange(c.proposeDeploy(c.dev, nil).ID, stPreparing)
	var stored CloudChange
	c.srv.docs.Get(context.Background(), kindCloud, ch.ID, &stored)
	stored.ProposedAt = time.Now().Add(-25 * time.Hour)
	c.srv.docs.Put(context.Background(), kindCloud, ch.ID, stored)

	var got CloudChange
	json.Unmarshal(c.view.expect(200, "GET", "/v1/cloud/changes/"+ch.ID, nil), &got)
	if got.Status != stExpired {
		t.Fatalf("status = %s, want expired", got.Status)
	}
	resp, body := c.admin2.do("POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]string{})
	if resp.StatusCode != 409 || !strings.Contains(string(body), "24 hours") {
		t.Fatalf("approving an expired proposal = %d %s", resp.StatusCode, body)
	}
}

func TestInstanceActionsAndStackDeletion(t *testing.T) {
	c := newCloudHarness(t)
	c.connectAWS()
	c.cli.stacks["forgeline-demo"] = "CREATE_COMPLETE"

	// Starting a stopped instance.
	var ch CloudChange
	json.Unmarshal(c.dev.expect(202, "POST", "/v1/cloud/changes", map[string]any{
		"kind": "instance-action", "instance_id": "i-0aaa1111bbbb2222c", "action": "start", "reason": "demo for the team"}), &ch)
	ch = c.waitChange(ch.ID, stPreparing)
	if ch.Status != stProposed || len(ch.Preview) != 1 || !strings.Contains(ch.Preview[0].Detail, "now stopped") {
		t.Fatalf("start proposal = %+v", ch)
	}
	c.admin2.expect(202, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]string{})
	ch = c.waitChange(ch.ID, stProposed, stExecuting)
	if ch.Status != stSucceeded || c.cli.instances["i-0aaa1111bbbb2222c"] != "running" {
		t.Fatalf("after start: %+v, state %s", ch, c.cli.instances["i-0aaa1111bbbb2222c"])
	}

	// A state that makes the action meaningless is caught at proposal time.
	json.Unmarshal(c.dev.expect(202, "POST", "/v1/cloud/changes", map[string]any{
		"kind": "instance-action", "instance_id": "i-0aaa1111bbbb2222c", "action": "start", "reason": "again, by mistake"}), &ch)
	ch = c.waitChange(ch.ID, stPreparing)
	if ch.Status != stFailed || !strings.Contains(ch.Result, "only a stopped instance can be started") {
		t.Fatalf("start of a running instance: %+v", ch)
	}
	json.Unmarshal(c.dev.expect(202, "POST", "/v1/cloud/changes", map[string]any{
		"kind": "instance-action", "instance_id": "i-0bad0bad0bad0bad0", "action": "stop", "reason": "cost saving"}), &ch)
	if ch = c.waitChange(ch.ID, stPreparing); ch.Status != stFailed {
		t.Fatalf("unknown instance: %+v", ch)
	}

	// Deleting a stack lists what goes with it, then does it.
	json.Unmarshal(c.dev.expect(202, "POST", "/v1/cloud/changes", map[string]any{
		"kind": "stack-delete", "stack": "forgeline-demo", "reason": "tearing the demo down"}), &ch)
	ch = c.waitChange(ch.ID, stPreparing)
	if ch.Status != stProposed || len(ch.Preview) != 1 || ch.Preview[0].Action != "Remove" {
		t.Fatalf("delete proposal = %+v", ch)
	}
	c.admin2.expect(202, "POST", "/v1/cloud/changes/"+ch.ID+"/approve", map[string]string{})
	ch = c.waitChange(ch.ID, stProposed, stExecuting)
	if ch.Status != stSucceeded {
		t.Fatalf("delete result = %+v", ch)
	}
	if _, exists := c.cli.stacks["forgeline-demo"]; exists {
		t.Error("the stack still exists")
	}
}

func TestOnlyOneChangeRunsAgainstATargetAtATime(t *testing.T) {
	c := newCloudHarness(t)
	c.connectAWS()
	c.cli.stacks["forgeline-demo"] = "CREATE_COMPLETE"
	c.srv.cloud.setInterval(200 * time.Millisecond) // keep the first one running while we try the second

	mk := func() CloudChange {
		var ch CloudChange
		json.Unmarshal(c.dev.expect(202, "POST", "/v1/cloud/changes", map[string]any{
			"kind": "stack-delete", "stack": "forgeline-demo", "reason": "tearing the demo down"}), &ch)
		return c.waitChange(ch.ID, stPreparing)
	}
	a, b := mk(), mk()
	c.admin2.expect(202, "POST", "/v1/cloud/changes/"+a.ID+"/approve", map[string]string{})
	resp, body := c.admin2.do("POST", "/v1/cloud/changes/"+b.ID+"/approve", map[string]string{})
	if resp.StatusCode != 409 || !strings.Contains(string(body), "already running") {
		t.Fatalf("second approval on one stack = %d %s", resp.StatusCode, body)
	}
	c.srv.cloud.setInterval(5 * time.Millisecond)
	c.waitChange(a.ID, stProposed, stExecuting)
}

func TestInterruptedChangesAreMarkedFailedOnRestart(t *testing.T) {
	c := newCloudHarness(t)
	ctx := context.Background()
	c.srv.docs.Put(ctx, kindCloud, "stuck", CloudChange{ID: "stuck", Kind: kindStackDeploy, Stack: "x", Status: stExecuting, Preview: []ChangePreview{}})
	c.srv.docs.Put(ctx, kindCloud, "prep", CloudChange{ID: "prep", Kind: kindStackDeploy, Stack: "y", Status: stPreparing, Preview: []ChangePreview{}})
	c.srv.failInterruptedChanges(ctx)
	for _, id := range []string{"stuck", "prep"} {
		var got CloudChange
		c.srv.docs.Get(ctx, kindCloud, id, &got)
		if got.Status != stFailed || !strings.Contains(got.Result, "restarted") {
			t.Errorf("%s = %+v", id, got)
		}
	}
}

func TestCatalogMatchesTheEmbeddedTemplate(t *testing.T) {
	// Every template parameter must be in the catalog (so the form can set it), and the catalog
	// must not offer parameters the template does not have (AWS would reject the change).
	block := awsdeploy.DemoTemplate[strings.Index(awsdeploy.DemoTemplate, "\nParameters:")+1:]
	block = block[:strings.Index(block, "\nResources:")]
	re := regexp.MustCompile(`(?m)^  ([A-Za-z0-9]+):\s*$`)
	inTemplate := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(block, -1) {
		inTemplate[m[1]] = true
	}
	// AmiId is resolved by AWS from SSM and is deliberately not exposed.
	inCatalog := map[string]bool{"AmiId": true}
	for _, p := range demoTemplate.Parameters {
		inCatalog[p.Name] = true
	}
	for name := range inTemplate {
		if !inCatalog[name] {
			t.Errorf("template parameter %s is not in the catalog", name)
		}
	}
	for name := range inCatalog {
		if !inTemplate[name] {
			t.Errorf("catalog parameter %s is not in the template", name)
		}
	}
	_ = fmt.Sprint
}

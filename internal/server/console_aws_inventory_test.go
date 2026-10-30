package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func specByID(t *testing.T, id string) invSpec {
	t.Helper()
	for _, sp := range inventorySpecs() {
		if sp.id == id {
			return sp
		}
	}
	t.Fatalf("no spec %q", id)
	return invSpec{}
}

func TestParseInventoryEC2ReadsNestedInstancesAndNameTag(t *testing.T) {
	raw := `{"Reservations":[{"Instances":[{"InstanceId":"i-1","InstanceType":"t3.small","State":{"Name":"running"},"PrivateIpAddress":"10.0.0.4","LaunchTime":"2026-10-01T00:00:00Z","Tags":[{"Key":"Name","Value":"web-1"},{"Key":"env","Value":"prod"}]},{"InstanceId":"i-2","State":{"Name":"stopped"}}]},{"Instances":[{"InstanceId":"i-3","State":{"Name":"running"}}]}]}`
	res, err := parseInventory(specByID(t, "ec2"), []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("got %d instances, want 3 across both reservations", len(res))
	}
	a := res[0]
	if a.ID != "i-1" || a.Name != "web-1" || a.State != "running" || a.Tags["env"] != "prod" {
		t.Errorf("first = %+v", a)
	}
	if res[1].Name != "i-2" {
		t.Errorf("an instance without a Name tag should be shown by id, got %q", res[1].Name)
	}
	got := map[string]string{}
	for _, f := range a.Fields {
		got[f.Label] = f.Value
	}
	if got["Type"] != "t3.small" || got["Private IP"] != "10.0.0.4" {
		t.Errorf("fields = %v", got)
	}
	if _, ok := got["Public IP"]; ok {
		t.Error("an empty field should be left out, not shown blank")
	}
}

func TestParseInventoryPlainStringsAndARNs(t *testing.T) {
	res, err := parseInventory(specByID(t, "dynamodb"), []byte(`{"TableNames":["orders","users"]}`))
	if err != nil || len(res) != 2 || res[0].Name != "orders" || res[0].ID != "orders" {
		t.Fatalf("dynamodb: %v %+v", err, res)
	}
	res, _ = parseInventory(specByID(t, "sqs"), []byte(`{"QueueUrls":["https://sqs.ap-south-1.amazonaws.com/111122223333/jobs"]}`))
	if len(res) != 1 || res[0].Name != "jobs" {
		t.Errorf("sqs name should be the last segment of the url: %+v", res)
	}
	res, _ = parseInventory(specByID(t, "sns"), []byte(`{"Topics":[{"TopicArn":"arn:aws:sns:ap-south-1:111122223333:alerts"}]}`))
	if len(res) != 1 || res[0].Name != "alerts" || res[0].ARN == "" {
		t.Errorf("sns: %+v", res)
	}
	if res, err := parseInventory(specByID(t, "sqs"), []byte("\n")); err != nil || len(res) != 0 {
		t.Errorf("a command that prints nothing means nothing is there: %v %+v", err, res)
	}
	// A service with nothing in it answers with no key at all.
	res, err = parseInventory(specByID(t, "dynamodb"), []byte(`{}`))
	if err != nil || len(res) != 0 {
		t.Errorf("empty answer: %v %+v", err, res)
	}
}

func TestParseInventoryEpochTimesAndMapTags(t *testing.T) {
	res, _ := parseInventory(specByID(t, "ecr"), []byte(`{"repositories":[{"repositoryName":"api","createdAt":1761782400.5,"repositoryUri":"u","imageScanningConfiguration":{"scanOnPush":true}}]}`))
	if len(res) != 1 || !strings.HasPrefix(res[0].Created, "2025-10-30") {
		t.Fatalf("epoch seconds should become a timestamp: %+v", res)
	}
	var scan string
	for _, f := range res[0].Fields {
		if f.Label == "Scan on push" {
			scan = f.Value
		}
	}
	if scan != "true" {
		t.Errorf("nested bool field = %q", scan)
	}
	res, _ = parseInventory(specByID(t, "logs"), []byte(`{"logGroups":[{"logGroupName":"/aws/x","creationTime":1761782400000,"tags":{"team":"core"}}]}`))
	if len(res) != 1 || !strings.HasPrefix(res[0].Created, "2025-10-30") || res[0].Tags["team"] != "core" {
		t.Errorf("milliseconds and map tags: %+v", res)
	}
}

func TestParseInventoryRejectsTheWrongShape(t *testing.T) {
	if _, err := parseInventory(specByID(t, "ec2"), []byte(`[1,2]`)); err == nil {
		t.Error("an array where an object is expected should be an error")
	}
	if _, err := parseInventory(specByID(t, "ec2"), []byte(`not json`)); err == nil {
		t.Error("bad JSON should be an error")
	}
}

// awsStub answers the CLI calls the explorer makes and records them.
type awsStub struct {
	mu    sync.Mutex
	calls []string
	deny  map[string]bool // "ecs list-services" -> AccessDenied
}

func (a *awsStub) run(_ context.Context, name string, args ...string) ([]byte, string, error) {
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--profile" || args[i] == "--region" || args[i] == "--output" {
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	key := strings.Join(rest, " ")
	a.mu.Lock()
	a.calls = append(a.calls, key)
	a.mu.Unlock()
	if len(rest) >= 2 && a.deny[rest[0]+" "+rest[1]] {
		return nil, "An error occurred (AccessDeniedException) when calling the " + rest[1] + " operation: not authorized", errors.New("exit 254")
	}
	switch {
	case strings.HasPrefix(key, "sts get-caller-identity"):
		return []byte(`{"Account":"111122223333","Arn":"arn:aws:sts::111122223333:assumed-role/Admin/ada","UserId":"x"}`), "", nil
	case strings.HasPrefix(key, "ecs list-clusters"):
		return []byte(`{"clusterArns":["arn:aws:ecs:ap-south-1:111122223333:cluster/payments"]}`), "", nil
	case strings.HasPrefix(key, "ecs describe-clusters"):
		return []byte(`{"clusters":[{"clusterName":"payments","clusterArn":"arn:aws:ecs:ap-south-1:111122223333:cluster/payments","status":"ACTIVE","activeServicesCount":1,"runningTasksCount":3,"pendingTasksCount":0,"registeredContainerInstancesCount":0,"settings":[{"name":"containerInsights","value":"enabled"}]}]}`), "", nil
	case strings.HasPrefix(key, "ecs list-services"):
		return []byte(`{"serviceArns":["arn:aws:ecs:ap-south-1:111122223333:service/payments/web"]}`), "", nil
	case strings.HasPrefix(key, "ecs describe-services"):
		return []byte(`{"services":[{"serviceName":"web","status":"ACTIVE","desiredCount":3,"runningCount":3,"pendingCount":0,"launchType":"FARGATE","taskDefinition":"arn:aws:ecs:ap-south-1:111122223333:task-definition/web:7"}]}`), "", nil
	case strings.HasPrefix(key, "s3api list-buckets"):
		return []byte(`{"Buckets":[{"Name":"assets-111122223333","CreationDate":"2026-01-01T00:00:00Z"}]}`), "", nil
	}
	return []byte(`{}`), "", nil // every other service is empty
}

func (a *awsStub) count(prefix string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func serviceOf(t *testing.T, inv AWSInventoryJSON, id string) AWSServiceJSON {
	t.Helper()
	for _, s := range inv.Services {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no service %q in %d services", id, len(inv.Services))
	return AWSServiceJSON{}
}

func TestInventoryListsEveryServiceAndLinksEcsServicesToTheirCluster(t *testing.T) {
	m := newMCPHarness(t)
	stub := &awsStub{}
	m.srv.run = stub.run
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})

	var inv AWSInventoryJSON
	if err := json.Unmarshal(m.v.expect(200, "GET", "/v1/infra/aws/resources", nil), &inv); err != nil {
		t.Fatal(err)
	}
	if inv.Account != "111122223333" || !inv.Enabled {
		t.Errorf("header = %+v", inv)
	}
	cl := serviceOf(t, inv, "ecs")
	if cl.Count != 1 || cl.Resources[0].Name != "payments" || cl.Resources[0].State != "ACTIVE" {
		t.Fatalf("clusters = %+v", cl)
	}
	var insights string
	for _, f := range cl.Resources[0].Fields {
		if f.Label == "Container Insights" {
			insights = f.Value
		}
	}
	if insights != "enabled" {
		t.Errorf("container insights = %q", insights)
	}
	sv := serviceOf(t, inv, "ecs-services")
	if sv.Count != 1 || sv.Resources[0].Name != "web" {
		t.Fatalf("services = %+v", sv)
	}
	f := map[string]string{}
	for _, x := range sv.Resources[0].Fields {
		f[x.Label] = x.Value
	}
	if f["Cluster"] != "payments" || f["Desired"] != "3" || f["Task definition"] != "web:7" {
		t.Errorf("service fields = %v", f)
	}
	if b := serviceOf(t, inv, "s3"); !b.Global || b.Count != 1 {
		t.Errorf("s3 = %+v", b)
	}
	if e := serviceOf(t, inv, "lambda"); e.Count != 0 || e.Error != "" || len(e.Resources) != 0 {
		t.Errorf("an empty service is empty, not an error: %+v", e)
	}
	if len(inv.Services) < 15 {
		t.Errorf("only %d services listed", len(inv.Services))
	}
}

func TestInventoryOneDeniedServiceDoesNotHideTheRest(t *testing.T) {
	m := newMCPHarness(t)
	stub := &awsStub{deny: map[string]bool{"lambda list-functions": true, "ecs list-services": true}}
	m.srv.run = stub.run
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})
	var inv AWSInventoryJSON
	json.Unmarshal(m.v.expect(200, "GET", "/v1/infra/aws/resources", nil), &inv)
	if l := serviceOf(t, inv, "lambda"); l.Error == "" || !strings.Contains(l.Error, "not allowed") {
		t.Errorf("lambda should say it is not allowed, got %+v", l)
	}
	if sv := serviceOf(t, inv, "ecs-services"); sv.Error == "" {
		t.Errorf("services denied should be flagged: %+v", sv)
	}
	if cl := serviceOf(t, inv, "ecs"); cl.Count != 1 {
		t.Errorf("clusters must still be listed when services are denied: %+v", cl)
	}
	if inv.Error != "" {
		t.Errorf("one denied service must not fail the whole view: %q", inv.Error)
	}
}

func TestInventoryIsCachedAndRefreshSkipsTheCache(t *testing.T) {
	m := newMCPHarness(t)
	stub := &awsStub{}
	m.srv.run = stub.run
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})
	m.v.expect(200, "GET", "/v1/infra/aws/resources", nil)
	n := stub.count("s3api list-buckets")
	m.v.expect(200, "GET", "/v1/infra/aws/resources", nil)
	if got := stub.count("s3api list-buckets"); got != n {
		t.Errorf("second request called AWS again (%d -> %d)", n, got)
	}
	m.v.expect(200, "GET", "/v1/infra/aws/resources?refresh=1", nil)
	if got := stub.count("s3api list-buckets"); got != n+1 {
		t.Errorf("refresh should call AWS once more, got %d", got)
	}
}

func TestInventoryOnlyReads(t *testing.T) {
	m := newMCPHarness(t)
	stub := &awsStub{}
	m.srv.run = stub.run
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})
	m.v.expect(200, "GET", "/v1/infra/aws/resources", nil)
	for _, c := range stub.calls {
		f := strings.Fields(c)
		if len(f) < 2 {
			continue
		}
		op := f[1]
		if !(strings.HasPrefix(op, "list-") || strings.HasPrefix(op, "describe-") || op == "get-caller-identity") {
			t.Errorf("the explorer ran %q, which is not a read", c)
		}
	}
}

func TestInventoryWhenNotConnected(t *testing.T) {
	m := newMCPHarness(t)
	var inv AWSInventoryJSON
	json.Unmarshal(m.v.expect(200, "GET", "/v1/infra/aws/resources", nil), &inv)
	if inv.Enabled || len(inv.Services) != 0 {
		t.Errorf("not connected should be empty: %+v", inv)
	}
}

func TestInventoryUnreachableAccountIsOneClearError(t *testing.T) {
	m := newMCPHarness(t)
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})
	m.srv.run = func(context.Context, string, ...string) ([]byte, string, error) {
		return nil, "Unable to locate credentials", errors.New("exit 253")
	}
	var inv AWSInventoryJSON
	json.Unmarshal(m.v.expect(200, "GET", "/v1/infra/aws/resources?refresh=1", nil), &inv)
	if inv.Error == "" || len(inv.Services) != 0 {
		t.Errorf("want one error and no services: %+v", inv)
	}
}

func TestInventoryCapsBigServicesButKeepsTheCount(t *testing.T) {
	var svc AWSServiceJSON
	var res []AWSResourceJSON
	for i := 0; i < inventoryMax+50; i++ {
		res = append(res, AWSResourceJSON{ID: "x", Name: strings.Repeat("a", 1) + string(rune('a'+i%26)) + string(rune('a'+i/26%26))})
	}
	fillService(&svc, res)
	if svc.Count != inventoryMax+50 || len(svc.Resources) != inventoryMax || !svc.Truncated {
		t.Errorf("count %d, shown %d, truncated %v", svc.Count, len(svc.Resources), svc.Truncated)
	}
}

func TestInventoryEmptyServiceHasAnEmptyListNotNull(t *testing.T) {
	var svc AWSServiceJSON
	fillService(&svc, nil)
	b, _ := json.Marshal(svc)
	if !strings.Contains(string(b), `"resources":[]`) {
		t.Errorf("empty service should encode resources as [], got %s", b)
	}
}

func TestAWSResourcesLookupShowsTheExplorerViewAndSharesItsCache(t *testing.T) {
	m := newMCPHarness(t)
	stub := &awsStub{}
	m.srv.run = stub.run
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})

	m.ask(m.dev, lookupReply("aws_resources", map[string]any{}))
	res := m.llm.lastUser(1)
	for _, want := range []string{"Account 111122223333", "ECS clusters (1):", "- payments [ACTIVE]", "Running tasks: 3", "ECS services (1):", "- web", "Desired: 3", "S3 buckets (1):", "Lambda functions: none"} {
		if !strings.Contains(res, want) {
			t.Errorf("missing %q in:\n%s", want, res)
		}
	}
	n := stub.count("s3api list-buckets")
	m.v.expect(200, "GET", "/v1/infra/aws/resources", nil) // the console's own read is served from the same cache
	m.ask(m.dev, lookupReply("aws_resources", map[string]any{"service": "ecs"}))
	if got := stub.count("s3api list-buckets"); got != n {
		t.Errorf("the lookup and the console read AWS separately (%d -> %d)", n, got)
	}
	res = m.llm.lastUser(1)
	if strings.Contains(res, "S3 buckets") || !strings.Contains(res, "ECS services") {
		t.Errorf("service filter: %s", res)
	}
	m.ask(m.dev, lookupReply("aws_resources", map[string]any{"service": "nope"}))
	if !strings.Contains(m.llm.lastUser(1), "no service matches") {
		t.Errorf("unknown service: %s", m.llm.lastUser(1))
	}
	m.ask(m.dev, lookupReply("aws_resources", map[string]any{"account": "ghost"}))
	if !strings.Contains(m.llm.lastUser(1), "no such AWS account") {
		t.Errorf("unknown account: %s", m.llm.lastUser(1))
	}
}

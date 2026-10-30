package server

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestParseAWSCommandAllowsReadsAndRefusesEverythingElse(t *testing.T) {
	ok := map[string]string{
		"aws ecs list-clusters":                                           "ecs list-clusters",
		"ecs describe-services --cluster payments --services web":         "ecs describe-services --cluster payments --services web",
		`aws ec2 describe-instances --filters "Name=tag:Env,Values=prod"`: "ec2 describe-instances --filters Name=tag:Env,Values=prod",
		"aws sts get-caller-identity":                                     "sts get-caller-identity",
		"aws ecs list-tasks --cluster c --query taskArns":                 "ecs list-tasks --cluster c --query taskArns",
	}
	for in, want := range ok {
		got, _, err := parseAWSCommand(in)
		if err != nil || strings.Join(got, " ") != want {
			t.Errorf("%q -> %v, %v; want %q", in, got, err, want)
		}
	}
	if _, region, err := parseAWSCommand("aws ecs list-clusters --region eu-west-1"); err != nil || region != "eu-west-1" {
		t.Errorf("region = %q, %v", region, err)
	}
	refused := []string{
		"", "aws", "aws ecs", "aws --profile x ecs list-clusters",
		"aws ec2 run-instances --image-id ami-1",         // not a read
		"aws ec2 terminate-instances --instance-ids i-1", // not a read
		"aws iam create-user --user-name x",
		"aws s3 ls", "aws s3api get-object --bucket b --key k out", // blocked service / data
		"aws secretsmanager get-secret-value --secret-id x",
		"aws ssm get-parameter --name /x --with-decryption",
		"aws ssm get-parameters-by-path --path /x",
		"aws kms decrypt --key-id k", "aws kms list-keys", "aws secretsmanager list-secrets", "aws sts get-access-key-info --access-key-id AKIA1", "aws sso list-accounts --access-token x",
		"aws sts get-session-token", "aws sts assume-role --role-arn r --role-session-name s",
		"aws ecr get-login-password", "aws ecr get-authorization-token",
		"aws lambda get-function --function-name f", "aws logs get-log-events --log-group-name g --log-stream-name s",
		"aws ec2 get-password-data --instance-id i-1", "aws ec2 get-console-output --instance-id i-1",
		"aws ecs list-clusters --endpoint-url http://evil.example.com",
		"aws ecs list-clusters --profile other",
		"aws ecs list-clusters --output text", "aws ecs list-clusters --debug",
		"aws ecs list-clusters --cli-input-json file:///etc/passwd",
		"aws ecs describe-services --cluster file:///etc/passwd",
		"aws ecs describe-services --cluster=http://169.254.169.254/",
		"aws ecs list-clusters | cat", "aws ecs list-clusters; aws ecs list-services", "aws ecs list-clusters $(id)",
		"aws ecs list-clusters `id`", "aws ecs list-clusters > /tmp/x", "aws ecs list-clusters\naws ec2 describe-instances",
		"aws ecs list-clusters --region 'x y'", "aws ecs list-clusters --region",
		"aws configure list", "aws sso get-role-credentials --role-name r", "aws ecs list-clusters 'unclosed",
		"aws " + strings.Repeat("a", maxAWSCommand),
	}
	for _, in := range refused {
		if got, _, err := parseAWSCommand(in); err == nil {
			t.Errorf("%q was accepted as %v", in, got)
		}
	}
}

func TestAssistantCallAWSRunsAReadWithTheConsoleConnection(t *testing.T) {
	llm := newScriptedLLM(t, "", "There is one cluster, payments.")
	h, admin, dev, _ := assistantHarness(t, llm, "x")
	var mu sync.Mutex
	var seen [][]string
	calls := func() [][]string { mu.Lock(); defer mu.Unlock(); return append([][]string(nil), seen...) }
	reset := func() { mu.Lock(); seen = nil; mu.Unlock() }
	h.srv.run = func(_ context.Context, name string, args ...string) ([]byte, string, error) {
		mu.Lock()
		seen = append(seen, append([]string{name}, args...))
		mu.Unlock()
		return []byte(`{"clusterArns":["arn:aws:ecs:ap-south-1:111122223333:cluster/payments"]}`), "", nil
	}
	ask := func(c *browser, reply string) ChatReply {
		llm.mu.Lock()
		llm.replies, llm.reqs = []string{reply, "done"}, nil
		llm.mu.Unlock()
		var r ChatReply
		json.Unmarshal(c.expect(200, "POST", "/v1/assistant/chat", map[string]any{"message": "what clusters?"}), &r)
		return r
	}

	// Not connected: the model is told, nothing runs.
	ask(admin, lookupReply("call_aws", map[string]string{"cli_command": "aws ecs list-clusters"}))
	if !strings.Contains(llm.lastUser(1), "AWS is not connected") || len(calls()) != 0 {
		t.Fatalf("not-connected result = %s (calls %v)", llm.lastUser(1), calls())
	}

	admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})
	reset()
	r := ask(admin, lookupReply("call_aws", map[string]string{"cli_command": "aws ecs list-clusters --region eu-west-1"}))
	if len(r.Lookups) != 1 || !strings.Contains(llm.lastUser(1), "cluster/payments") {
		t.Fatalf("result = %s", llm.lastUser(1))
	}
	if len(calls()) != 1 {
		t.Fatalf("calls = %v", calls())
	}
	line := strings.Join(calls()[0], " ")
	if !strings.HasPrefix(line, "aws ") || !strings.Contains(line, "--profile dev") || strings.Count(line, "--region") != 1 || !strings.Contains(line, "--region eu-west-1") || !strings.HasSuffix(line, "--output json") {
		t.Errorf("command line = %q", line)
	}

	// A refused command never reaches the CLI, and the model is told why.
	reset()
	ask(admin, lookupReply("call_aws", map[string]string{"cli_command": "aws ecs delete-cluster --cluster payments"}))
	if len(calls()) != 0 || !strings.Contains(llm.lastUser(1), "not a read operation") {
		t.Errorf("a write reached the CLI: %v / %s", calls(), llm.lastUser(1))
	}

	// Developers cannot use it, and the prompt does not offer it to them.
	ask(dev, lookupReply("call_aws", map[string]string{"cli_command": "aws ecs list-clusters"}))
	if len(calls()) != 0 || !strings.Contains(llm.lastUser(1), "not available to your role") {
		t.Errorf("a developer ran call_aws: %v / %s", calls(), llm.lastUser(1))
	}
	llm.mu.Lock()
	sys := llm.reqs[0]["system"].(string)
	llm.mu.Unlock()
	if strings.Contains(sys, "call_aws") {
		t.Error("the developer's prompt advertises call_aws")
	}

	// The audit log records the command that was run.
	if a := string(admin.expect(200, "GET", "/v1/audit", nil)); !strings.Contains(a, "call_aws: aws ecs list-clusters --region eu-west-1") {
		t.Errorf("command not audited: %s", a)
	}
}

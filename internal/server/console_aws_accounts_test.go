package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// multiStub answers the CLI for several profiles, each its own AWS account, and records the calls.
type multiStub struct {
	mu       sync.Mutex
	accounts map[string]string // profile -> account id; a profile not listed cannot sign in
	calls    []string          // "profile: command"
	gate     chan struct{}     // when set, describe-instances waits for it to be closed
	stsGate  chan struct{}     // when set, the staging profile's sts call waits for it to be closed
}

func (m *multiStub) run(_ context.Context, name string, args ...string) ([]byte, string, error) {
	profile := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--profile":
			profile = args[i+1]
			i++
		case "--region", "--output":
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	m.mu.Lock()
	m.calls = append(m.calls, profile+": "+strings.Join(rest, " "))
	m.mu.Unlock()
	acct, ok := m.accounts[profile]
	if !ok {
		return nil, "The config profile (" + profile + ") could not be found", errors.New("exit 255")
	}
	joined := strings.Join(rest, " ")
	if m.stsGate != nil && profile == "staging" && joined == "sts get-caller-identity" {
		<-m.stsGate
	}
	if m.gate != nil && strings.HasPrefix(joined, "ec2 describe-instances") {
		<-m.gate
	}
	if strings.HasPrefix(joined, "ec2 describe-instances") {
		return []byte(`{"Reservations":[{"Instances":[{"InstanceType":"t3.small","State":{"Name":"running"},"Tags":[{"Key":"Name","Value":"web-` + acct + `"}]}]}]}`), "", nil
	}
	if strings.HasPrefix(joined, "ec2 reboot-instances") {
		return []byte(`{}`), "", nil
	}
	switch joined {
	case "sts get-caller-identity":
		return []byte(`{"Account":"` + acct + `","Arn":"arn:aws:sts::` + acct + `:assumed-role/Admin/ada","UserId":"x"}`), "", nil
	case "s3api list-buckets":
		return []byte(`{"Buckets":[{"Name":"bucket-of-` + acct + `","CreationDate":"2026-01-01T00:00:00Z"}]}`), "", nil
	}
	return []byte(`{}`), "", nil
}

func (m *multiStub) called(profile, prefix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if strings.HasPrefix(c, profile+": "+prefix) {
			n++
		}
	}
	return n
}

func accountHarness(t *testing.T) (*mcpHarness, *multiStub) {
	m := newMCPHarness(t)
	st := &multiStub{accounts: map[string]string{"dev": "111122223333", "staging": "444455556666", "prod": "777788889999"}}
	m.srv.run = st.run
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})
	return m, st
}

type acctList struct {
	Accounts []AWSAccountJSON `json:"accounts"`
}

func listAccounts(t *testing.T, c *browser) []AWSAccountJSON {
	t.Helper()
	var l acctList
	if err := json.Unmarshal(c.expect(200, "GET", "/v1/aws/accounts", nil), &l); err != nil {
		t.Fatal(err)
	}
	return l.Accounts
}

func addAccount(c *browser, want int, name, profile, env string) []byte {
	return c.expect(want, "POST", "/v1/aws/accounts", map[string]any{"name": name, "profile": profile, "region": "eu-west-1", "env": env})
}

func TestAccountsAddListRemove(t *testing.T) {
	m, _ := accountHarness(t)
	if l := listAccounts(t, m.v); len(l) != 1 || !l[0].Primary || l[0].ID != "primary" {
		t.Fatalf("before adding: %+v", l)
	}
	var a AWSAccountJSON
	json.Unmarshal(addAccount(m.admin, 201, "Staging EU", "staging", "staging"), &a)
	if a.ID != "staging-eu" || a.AccountID != "444455556666" || a.Env != "staging" {
		t.Errorf("added = %+v", a)
	}
	l := listAccounts(t, m.v)
	if len(l) != 2 || !l[0].Primary || l[1].Name != "Staging EU" {
		t.Errorf("list = %+v", l)
	}
	m.admin.expect(204, "DELETE", "/v1/aws/accounts/"+a.ID, nil)
	if l := listAccounts(t, m.v); len(l) != 1 {
		t.Errorf("after removing: %+v", l)
	}
	m.admin.expect(404, "DELETE", "/v1/aws/accounts/"+a.ID, nil)
	m.admin.expect(400, "DELETE", "/v1/aws/accounts/primary", nil)
}

func TestAccountsOnlyAdministratorsChangeThem(t *testing.T) {
	m, _ := accountHarness(t)
	addAccount(m.dev, 403, "Staging", "staging", "")
	addAccount(m.v, 403, "Staging", "staging", "")
	addAccount(m.admin, 201, "Staging", "staging", "")
	m.dev.expect(403, "DELETE", "/v1/aws/accounts/staging", nil)
	// Everyone who can read the console may see which accounts exist.
	if l := listAccounts(t, m.v); len(l) != 2 {
		t.Errorf("viewer sees %+v", l)
	}
}

func TestAccountsRefuseBadInputAndDuplicates(t *testing.T) {
	m, _ := accountHarness(t)
	post := func(want int, body map[string]any) string {
		return string(m.admin.expect(want, "POST", "/v1/aws/accounts", body))
	}
	ok := func() map[string]any {
		return map[string]any{"name": "Prod", "profile": "prod", "region": "eu-west-1", "env": "production"}
	}
	for name, mut := range map[string]func(map[string]any){
		"empty name":    func(b map[string]any) { b["name"] = " " },
		"long name":     func(b map[string]any) { b["name"] = strings.Repeat("x", accountNameMax+1) },
		"profile chars": func(b map[string]any) { b["profile"] = "a b;rm" },
		"empty profile": func(b map[string]any) { b["profile"] = "" },
		"sso profile":   func(b map[string]any) { b["profile"] = ssoProfile },
		"bad region":    func(b map[string]any) { b["region"] = "mars" },
		"bad env":       func(b map[string]any) { b["env"] = "yolo" },
	} {
		b := ok()
		mut(b)
		post(400, b)
		_ = name
	}
	if msg := post(422, map[string]any{"name": "Ghost", "profile": "nope", "region": "eu-west-1"}); !strings.Contains(msg, "profile does not exist") {
		t.Errorf("an unusable profile should say why: %s", msg)
	}
	if l := listAccounts(t, m.v); len(l) != 1 {
		t.Errorf("nothing should have been saved: %+v", l)
	}
	post(201, ok())
	other := ok()
	other["profile"] = "staging"
	if msg := post(409, other); !strings.Contains(msg, "already an account named") {
		t.Errorf("same name: %s", msg)
	}
	m.srv.run = (&multiStub{accounts: map[string]string{"dev": "111122223333", "alias": "777788889999"}}).run
	if msg := post(409, map[string]any{"name": "Prod again", "profile": "alias", "region": "eu-west-1"}); !strings.Contains(msg, "already added") {
		t.Errorf("same AWS account under another profile: %s", msg)
	}
	if msg := post(409, map[string]any{"name": "Same as primary", "profile": "dev", "region": "us-east-1"}); !strings.Contains(msg, "primary") {
		t.Errorf("the primary account again: %s", msg)
	}
}

func TestAccountsReadWithTheirOwnProfileAndCache(t *testing.T) {
	m, st := accountHarness(t)
	addAccount(m.admin, 201, "Staging", "staging", "staging")

	var inv AWSInventoryJSON
	json.Unmarshal(m.v.expect(200, "GET", "/v1/infra/aws/resources?account=staging", nil), &inv)
	if inv.Account != "444455556666" || serviceOf(t, inv, "s3").Resources[0].Name != "bucket-of-444455556666" {
		t.Errorf("staging explorer showed %+v", inv.Account)
	}
	var prim AWSInventoryJSON
	json.Unmarshal(m.v.expect(200, "GET", "/v1/infra/aws/resources", nil), &prim)
	if prim.Account != "111122223333" {
		t.Errorf("primary explorer showed account %q", prim.Account)
	}
	// Switching back and forth does not read again inside the cache time.
	n := st.called("staging", "s3api list-buckets")
	m.v.expect(200, "GET", "/v1/infra/aws/resources?account=staging", nil)
	m.v.expect(200, "GET", "/v1/infra/aws/resources", nil)
	m.v.expect(200, "GET", "/v1/infra/aws/resources?account=primary", nil)
	if got := st.called("staging", "s3api list-buckets"); got != n {
		t.Errorf("staging was read again: %d -> %d", n, got)
	}
	if st.called("dev", "s3api list-buckets") != 1 {
		t.Errorf("?account=primary and no parameter should share one cached read, got %d", st.called("dev", "s3api list-buckets"))
	}
	m.v.expect(404, "GET", "/v1/infra/aws/resources?account=ghost", nil)
	m.v.expect(404, "GET", "/v1/infra/aws?account=ghost", nil)
	m.dev.expect(404, "GET", "/v1/infra/aws/cost?account=ghost", nil)

	var sum AWSJSON
	json.Unmarshal(m.v.expect(200, "GET", "/v1/infra/aws?account=staging", nil), &sum)
	if sum.Identity == nil || sum.Identity.Account != "444455556666" {
		t.Errorf("stacks view for staging: %+v", sum.Identity)
	}
}

func TestAccountsTheAssistantCanListAndReadThem(t *testing.T) {
	m, st := accountHarness(t)
	addAccount(m.admin, 201, "Staging", "staging", "staging")

	r := m.ask(m.admin, "```forgeline-lookup\n{\"tool\":\"aws_accounts\",\"args\":{}}\n```")
	if len(r.Lookups) != 1 || r.Lookups[0] != "aws_accounts" {
		t.Fatalf("lookups = %v", r.Lookups)
	}
	if !strings.Contains(m.llm.lastUser(1), `id "staging"`) || !strings.Contains(m.llm.lastUser(1), "primary") {
		t.Errorf("the model was told: %s", m.llm.lastUser(1))
	}

	m.ask(m.admin, "```forgeline-lookup\n{\"tool\":\"aws_summary\",\"args\":{\"account\":\"staging\"}}\n```")
	if !strings.Contains(m.llm.lastUser(1), "account 444455556666") && !strings.Contains(m.llm.lastUser(1), "staging") {
		t.Errorf("summary for staging: %s", m.llm.lastUser(1))
	}
	if st.called("staging", "sts get-caller-identity") == 0 {
		t.Error("the summary should have been read with the staging profile")
	}

	m.ask(m.admin, "```forgeline-lookup\n{\"tool\":\"aws_summary\",\"args\":{\"account\":\"ghost\"}}\n```")
	if !strings.Contains(m.llm.lastUser(1), "no such AWS account") {
		t.Errorf("unknown account: %s", m.llm.lastUser(1))
	}
}

func TestAccountsCallAWSWhileTheMCPServerOwnsThePrimary(t *testing.T) {
	m, st := accountHarness(t)
	addAccount(m.admin, 201, "Staging", "staging", "staging")
	m.admin.expect(200, "PUT", "/v1/integrations/aws/mcp", map[string]any{"enabled": true})

	// For the primary account the MCP server is the one way in.
	m.ask(m.admin, "```forgeline-lookup\n{\"tool\":\"call_aws\",\"args\":{\"cli_command\":\"aws s3api list-buckets\"}}\n```")
	if !strings.Contains(m.llm.lastUser(1), "aws_script") {
		t.Errorf("primary should point to aws_script: %s", m.llm.lastUser(1))
	}
	// An added account has no MCP path, so the CLI is allowed for it.
	m.ask(m.admin, "```forgeline-lookup\n{\"tool\":\"call_aws\",\"args\":{\"cli_command\":\"aws s3api list-buckets\",\"account\":\"staging\"}}\n```")
	if !strings.Contains(m.llm.lastUser(1), "bucket-of-444455556666") {
		t.Errorf("staging read: %s", m.llm.lastUser(1))
	}
	if st.called("staging", "s3api list-buckets") != 1 {
		t.Error("the call should have used the staging profile")
	}
	// It is still only reads, and still admins only.
	m.ask(m.admin, "```forgeline-lookup\n{\"tool\":\"call_aws\",\"args\":{\"cli_command\":\"aws s3api delete-bucket --bucket x\",\"account\":\"staging\"}}\n```")
	if st.called("staging", "s3api delete-bucket") != 0 {
		t.Error("a write ran against an added account")
	}
	m.ask(m.dev, "```forgeline-lookup\n{\"tool\":\"call_aws\",\"args\":{\"cli_command\":\"aws s3api list-buckets\",\"account\":\"staging\"}}\n```")
	if st.called("staging", "s3api list-buckets") != 1 {
		t.Error("a developer ran call_aws")
	}
	// The audit log says which account.
	if !strings.Contains(string(m.admin.expect(200, "GET", "/v1/audit?limit=50", nil)), "call_aws [staging]") {
		t.Error("the audit entry should name the account")
	}
}

func TestAccountsHelpOffersCallAWSOnlyWhenThereIsSomethingForItToReach(t *testing.T) {
	m, _ := accountHarness(t)
	m.admin.expect(200, "PUT", "/v1/integrations/aws/mcp", map[string]any{"enabled": true})
	ctx := context.Background()
	if strings.Contains(m.srv.lookupHelp(ctx, "admin"), "call_aws") {
		t.Error("with the MCP server on and one account, call_aws has nothing to reach")
	}
	addAccount(m.admin, 201, "Staging", "staging", "")
	if !strings.Contains(m.srv.lookupHelp(ctx, "admin"), "call_aws") {
		t.Error("with an added account, call_aws should be offered")
	}
}

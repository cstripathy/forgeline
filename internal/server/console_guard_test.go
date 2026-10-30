package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// guardStub is a messy AWS account for the CLI: one of each problem, and some clean resources.
type guardStub struct {
	mu       sync.Mutex
	deny     map[string]bool // "ec2 describe-security-groups" -> AccessDenied
	noWorld  bool            // drop the all-traffic group, to see a finding resolve
	block    chan struct{}   // when set, the identity call waits (to hold a scan open)
	accounts map[string]string
	calls    []string
}

func (g *guardStub) credentialReport(now time.Time) string {
	old := now.AddDate(0, 0, -200).Format(time.RFC3339)
	fresh := now.AddDate(0, 0, -5).Format(time.RFC3339)
	csv := "user,arn,password_enabled,mfa_active,access_key_1_active,access_key_1_last_rotated,access_key_2_active,access_key_2_last_rotated\n" +
		"<root_account>,arn:root,not_supported,false,true," + fresh + ",false,N/A\n" +
		"alice,arn:alice,true,false,false,N/A,false,N/A\n" +
		"bob,arn:bob,false,false,true," + old + ",false,N/A\n" +
		"carol,arn:carol,true,true,true," + fresh + ",false,N/A\n"
	return base64.StdEncoding.EncodeToString([]byte(csv))
}

func (g *guardStub) run(_ context.Context, name string, args ...string) ([]byte, string, error) {
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
	key := strings.Join(rest, " ")
	g.mu.Lock()
	g.calls = append(g.calls, profile+": "+key)
	deny, noWorld, block := g.deny, g.noWorld, g.block
	g.mu.Unlock()
	if block != nil && strings.HasPrefix(key, "sts get-caller-identity") {
		<-block
	}
	if len(rest) >= 2 && deny[rest[0]+" "+rest[1]] {
		return nil, "An error occurred (AccessDenied) when calling the " + rest[1] + " operation: not authorized", errors.New("exit 254")
	}
	acct := "111122223333"
	if a, ok := g.accounts[profile]; ok {
		acct = a
	}
	now := time.Now().UTC()
	switch {
	case strings.HasPrefix(key, "sts get-caller-identity"):
		return []byte(`{"Account":"` + acct + `","Arn":"arn:aws:sts::` + acct + `:assumed-role/Admin/ada","UserId":"x"}`), "", nil
	case strings.HasPrefix(key, "ec2 describe-security-groups"):
		world := `{"IpProtocol":"-1","IpRanges":[{"CidrIp":"0.0.0.0/0"}],"Ipv6Ranges":[]}`
		groups := []string{
			`{"GroupId":"sg-ssh","GroupName":"ssh-open","IpPermissions":[{"IpProtocol":"tcp","FromPort":22,"ToPort":22,"IpRanges":[{"CidrIp":"0.0.0.0/0"}]},{"IpProtocol":"tcp","FromPort":443,"ToPort":443,"IpRanges":[{"CidrIp":"0.0.0.0/0"}]}]}`,
			`{"GroupId":"sg-web","GroupName":"web-only","IpPermissions":[{"IpProtocol":"tcp","FromPort":443,"ToPort":443,"IpRanges":[{"CidrIp":"0.0.0.0/0"}]},{"IpProtocol":"tcp","FromPort":80,"ToPort":80,"Ipv6Ranges":[{"CidrIpv6":"::/0"}]}]}`,
			`{"GroupId":"sg-private","GroupName":"internal","IpPermissions":[{"IpProtocol":"tcp","FromPort":22,"ToPort":22,"IpRanges":[{"CidrIp":"10.0.0.0/8"}]}]}`,
		}
		if !noWorld {
			groups = append(groups, `{"GroupId":"sg-all","GroupName":"wide-open","IpPermissions":[`+world+`]}`)
		}
		return []byte(`{"SecurityGroups":[` + strings.Join(groups, ",") + `]}`), "", nil
	case strings.HasPrefix(key, "ec2 describe-volumes"):
		return []byte(`{"Volumes":[{"VolumeId":"vol-plain","State":"in-use","Encrypted":false,"Size":20,"VolumeType":"gp3"},{"VolumeId":"vol-enc","State":"in-use","Encrypted":true,"Size":8,"VolumeType":"gp3"}]}`), "", nil
	case strings.HasPrefix(key, "ec2 describe-instances"):
		old := now.AddDate(0, 0, -45).Format(time.RFC3339)
		return []byte(`{"Reservations":[{"Instances":[{"InstanceId":"i-old","InstanceType":"t3.small","State":{"Name":"stopped"},"LaunchTime":"` + old + `"},{"InstanceId":"i-run","State":{"Name":"running"}}]}]}`), "", nil
	case strings.HasPrefix(key, "rds describe-db-instances"):
		return []byte(`{"DBInstances":[{"DBInstanceIdentifier":"orders","Engine":"postgres","PubliclyAccessible":true,"StorageEncrypted":false,"DBInstanceStatus":"available"},{"DBInstanceIdentifier":"safe","Engine":"postgres","PubliclyAccessible":false,"StorageEncrypted":true,"DBInstanceStatus":"available"}]}`), "", nil
	case strings.HasPrefix(key, "cloudformation describe-stacks"):
		return []byte(`{"Stacks":[{"StackName":"broken","StackStatus":"ROLLBACK_COMPLETE"},{"StackName":"fine","StackStatus":"CREATE_COMPLETE"}]}`), "", nil
	case strings.HasPrefix(key, "ecs list-clusters"):
		return []byte(`{"clusterArns":["arn:aws:ecs:ap-south-1:1:cluster/empty"]}`), "", nil
	case strings.HasPrefix(key, "ecs describe-clusters"):
		return []byte(`{"clusters":[{"clusterName":"empty","status":"ACTIVE","activeServicesCount":0,"runningTasksCount":0,"pendingTasksCount":0,"registeredContainerInstancesCount":0}]}`), "", nil
	case strings.HasPrefix(key, "ecs list-services"):
		return []byte(`{"serviceArns":[]}`), "", nil
	case strings.HasPrefix(key, "logs describe-log-groups"):
		return []byte(`{"logGroups":[{"logGroupName":"/forever"},{"logGroupName":"/kept","retentionInDays":30}]}`), "", nil
	case strings.HasPrefix(key, "ecr describe-repositories"):
		return []byte(`{"repositories":[{"repositoryName":"noscan","imageScanningConfiguration":{"scanOnPush":false}},{"repositoryName":"scanned","imageScanningConfiguration":{"scanOnPush":true}}]}`), "", nil
	case strings.HasPrefix(key, "lambda list-functions"):
		return []byte(`{"Functions":[{"FunctionName":"legacy","Runtime":"python3.7"},{"FunctionName":"modern","Runtime":"python3.12"}]}`), "", nil
	case strings.HasPrefix(key, "s3api list-buckets"):
		return []byte(`{"Buckets":[{"Name":"open-bucket","CreationDate":"2026-01-01T00:00:00Z"},{"Name":"half-bucket","CreationDate":"2026-01-01T00:00:00Z"},{"Name":"tight-bucket","CreationDate":"2026-01-01T00:00:00Z"}]}`), "", nil
	case strings.HasPrefix(key, "s3api get-public-access-block"):
		switch rest[len(rest)-1] {
		case "open-bucket":
			return nil, "An error occurred (NoSuchPublicAccessBlockConfiguration) when calling the GetPublicAccessBlock operation", errors.New("exit 254")
		case "half-bucket":
			return []byte(`{"PublicAccessBlockConfiguration":{"BlockPublicAcls":true,"IgnorePublicAcls":true,"BlockPublicPolicy":false,"RestrictPublicBuckets":true}}`), "", nil
		}
		return []byte(`{"PublicAccessBlockConfiguration":{"BlockPublicAcls":true,"IgnorePublicAcls":true,"BlockPublicPolicy":true,"RestrictPublicBuckets":true}}`), "", nil
	case strings.HasPrefix(key, "iam generate-credential-report"):
		return []byte(`{"State":"COMPLETE"}`), "", nil
	case strings.HasPrefix(key, "iam get-credential-report"):
		return []byte(`{"Content":"` + g.credentialReport(now) + `"}`), "", nil
	}
	return []byte(`{}`), "", nil
}

func guardHarness(t *testing.T) (*mcpHarness, *guardStub) {
	m := newMCPHarness(t)
	st := &guardStub{deny: map[string]bool{}, accounts: map[string]string{"staging": "444455556666"}}
	m.srv.run = st.run
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": true, "profile": "dev", "region": "ap-south-1"})
	return m, st
}

type findingsOut struct {
	Findings []GuardFindingView `json:"findings"`
	Counts   map[string]int     `json:"counts"`
	Accepted int                `json:"accepted"`
}

func (m *mcpHarness) findings(query string) findingsOut {
	m.t.Helper()
	var out findingsOut
	json.Unmarshal(m.v.expect(200, "GET", "/v1/guard/findings"+query, nil), &out)
	return out
}

func (m *mcpHarness) scan(body map[string]any) GuardScan {
	m.t.Helper()
	var sc GuardScan
	json.Unmarshal(m.dev.expect(202, "POST", "/v1/guard/scan", body), &sc)
	m.waitFor("the scan to finish", func() bool {
		var out struct{ Scans []GuardScan }
		json.Unmarshal(m.v.expect(200, "GET", "/v1/guard/scans", nil), &out)
		for _, s := range out.Scans {
			if s.ID == sc.ID && s.Status != "running" {
				sc = s
				return true
			}
		}
		return false
	})
	return sc
}

func ruleSet(fs []GuardFindingView) map[string]GuardFindingView {
	out := map[string]GuardFindingView{}
	for _, f := range fs {
		out[f.Rule+"|"+f.Resource] = f
	}
	return out
}

func TestGuardFindsEachProblemAndNothingElse(t *testing.T) {
	m, _ := guardHarness(t)
	sc := m.scan(map[string]any{})
	if sc.Status != "done" || len(sc.Accounts) != 1 || sc.Accounts[0].Error != "" || len(sc.Accounts[0].Skipped) != 0 {
		t.Fatalf("scan = %+v", sc)
	}
	got := ruleSet(m.findings("").Findings)
	want := map[string]string{
		"sg-open-all|sg-all": sevCritical, "sg-open-ports|sg-ssh": sevHigh, "ebs-unencrypted|vol-plain": sevMedium,
		"rds-public|orders": sevHigh, "rds-unencrypted|orders": sevMedium, "cfn-failed-stack|broken": sevLow, "ecs-idle-cluster|empty": sevInfo,
		"ec2-stopped|i-old": sevLow, "logs-no-retention|/forever": sevLow, "ecr-no-scan|noscan": sevLow, "lambda-old-runtime|legacy": sevMedium,
		"s3-no-public-block|open-bucket": sevHigh, "s3-partial-public-block|half-bucket": sevMedium,
		"iam-root-keys|root": sevCritical, "iam-root-no-mfa|root": sevHigh, "iam-user-no-mfa|alice": sevHigh, "iam-old-access-key|bob/key1": sevMedium,
	}
	for k, sev := range want {
		f, ok := got[k]
		if !ok {
			t.Errorf("missing finding %s", k)
			continue
		}
		if f.Severity != sev || f.Fix == "" || f.Title == "" || f.Status != "open" || f.AccountName != "Primary" || f.AWSAccount != "111122223333" {
			t.Errorf("%s = %+v", k, f.GuardFinding)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected finding %s", k)
		}
	}
	// The clean ones must not be reported.
	for _, clean := range []string{"sg-web", "sg-private", "vol-enc", "safe", "fine", "/kept", "scanned", "modern", "tight-bucket", "carol", "i-run"} {
		for k := range got {
			if strings.HasSuffix(k, "|"+clean) || strings.Contains(k, "|"+clean+"/") {
				t.Errorf("clean resource reported: %s", k)
			}
		}
	}
	out := m.findings("")
	if out.Counts[sevCritical] != 2 || out.Counts[sevHigh] != 5 || out.Counts[sevMedium] != 5 {
		t.Errorf("counts = %v", out.Counts)
	}
	if out.Findings[0].Severity != sevCritical || out.Findings[len(out.Findings)-1].Severity != sevInfo {
		t.Errorf("not worst first: %s ... %s", out.Findings[0].Severity, out.Findings[len(out.Findings)-1].Severity)
	}
	if len(m.findings("?severity=critical").Findings) != 2 || len(m.findings("?account=nope").Findings) != 0 {
		t.Error("filters do not work")
	}
}

func TestGuardKeepsHistoryAndResolvesWhatIsGone(t *testing.T) {
	m, st := guardHarness(t)
	m.scan(map[string]any{})
	first := ruleSet(m.findings("").Findings)["sg-open-all|sg-all"]
	time.Sleep(20 * time.Millisecond)
	m.scan(map[string]any{})
	again := ruleSet(m.findings("").Findings)["sg-open-all|sg-all"]
	if again.ID != first.ID || !again.FirstSeen.Equal(first.FirstSeen) || !again.LastSeen.After(first.LastSeen) {
		t.Errorf("a finding that persists keeps its id and first-seen: %+v then %+v", first.GuardFinding, again.GuardFinding)
	}
	// The group is fixed: the finding resolves and moves to the resolved list.
	st.mu.Lock()
	st.noWorld = true
	st.mu.Unlock()
	m.scan(map[string]any{})
	if _, ok := ruleSet(m.findings("").Findings)["sg-open-all|sg-all"]; ok {
		t.Error("a fixed problem is still open")
	}
	res := ruleSet(m.findings("?status=resolved").Findings)["sg-open-all|sg-all"]
	if res.Status != "resolved" || res.ResolvedAt == nil || !res.FirstSeen.Equal(first.FirstSeen) {
		t.Errorf("resolved = %+v", res.GuardFinding)
	}
	// It comes back: same id, reopened, original first-seen kept.
	st.mu.Lock()
	st.noWorld = false
	st.mu.Unlock()
	m.scan(map[string]any{})
	back := ruleSet(m.findings("").Findings)["sg-open-all|sg-all"]
	if back.ID != first.ID || back.Status != "open" || !back.FirstSeen.Equal(first.FirstSeen) || back.ResolvedAt != nil {
		t.Errorf("reopened = %+v", back.GuardFinding)
	}
}

func TestGuardNeverResolvesWhatItCouldNotCheck(t *testing.T) {
	m, st := guardHarness(t)
	m.scan(map[string]any{})
	before := len(m.findings("").Findings)
	// Security groups, S3 public access and IAM become unreadable. Their findings must stay open.
	st.mu.Lock()
	st.deny = map[string]bool{"ec2 describe-security-groups": true, "s3api get-public-access-block": true, "iam generate-credential-report": true}
	st.mu.Unlock()
	sc := m.scan(map[string]any{})
	if got := len(m.findings("").Findings); got != before {
		t.Errorf("findings fell from %d to %d when checks could not run", before, got)
	}
	for _, f := range m.findings("?status=resolved").Findings {
		t.Errorf("resolved without being checked: %s %s (service %s)", f.Rule, f.Resource, f.Service)
	}
	skipped := strings.Join(sc.Accounts[0].Skipped, " | ")
	for _, want := range []string{"Security groups", "S3 public access", "IAM credentials"} {
		if !strings.Contains(skipped, want) {
			t.Errorf("the scan did not say it skipped %q: %s", want, skipped)
		}
	}
}

func TestGuardExceptions(t *testing.T) {
	m, _ := guardHarness(t)
	m.scan(map[string]any{})
	f := ruleSet(m.findings("").Findings)["sg-open-ports|sg-ssh"]
	path := "/v1/guard/findings/" + f.ID + "/accept"
	ok := map[string]any{"reason": "Office VPN range is being added next sprint", "days": 14}
	m.dev.expect(403, "POST", path, ok)
	m.v.expect(403, "POST", path, ok)
	m.admin.expect(400, "POST", path, map[string]any{"reason": "short", "days": 14})
	m.admin.expect(400, "POST", path, map[string]any{"reason": "A perfectly good reason here", "days": 0})
	m.admin.expect(400, "POST", path, map[string]any{"reason": "A perfectly good reason here", "days": maxExceptionDays + 1})
	m.admin.expect(404, "POST", "/v1/guard/findings/nothing/accept", ok)
	before := m.findings("")
	m.admin.expect(200, "POST", path, ok)
	after := m.findings("")
	got := ruleSet(after.Findings)["sg-open-ports|sg-ssh"]
	if got.Accepted == nil || got.Accepted.By != "ada" || !strings.Contains(got.Accepted.Reason, "Office VPN") || got.Accepted.Expires.Before(time.Now().Add(13*24*time.Hour)) {
		t.Fatalf("accepted = %+v", got.Accepted)
	}
	if after.Counts[sevHigh] != before.Counts[sevHigh]-1 || after.Accepted != 1 {
		t.Errorf("an accepted finding should leave the counts: %v then %v (accepted %d)", before.Counts, after.Counts, after.Accepted)
	}
	if after.Findings[len(after.Findings)-1].Accepted == nil {
		t.Error("accepted findings sort last")
	}
	// When the acceptance runs out the finding counts again.
	e := *got.Accepted
	e.Expires = time.Now().Add(-time.Hour)
	m.srv.docs.Put(context.Background(), kindGuardExc, e.ID, e)
	back := m.findings("")
	if ruleSet(back.Findings)["sg-open-ports|sg-ssh"].Accepted != nil || back.Counts[sevHigh] != before.Counts[sevHigh] {
		t.Errorf("an expired acceptance still hides the finding: %v", back.Counts)
	}
	// Accept again, then end it early.
	m.admin.expect(200, "POST", path, ok)
	m.dev.expect(403, "DELETE", path, nil)
	m.admin.expect(204, "DELETE", path, nil)
	m.admin.expect(404, "DELETE", path, nil)
	if ruleSet(m.findings("").Findings)["sg-open-ports|sg-ssh"].Accepted != nil {
		t.Error("still accepted after it was removed")
	}
	if !strings.Contains(string(m.admin.expect(200, "GET", "/v1/audit?limit=100", nil)), "guard.accept") {
		t.Error("acceptances are not audited")
	}
}

func TestGuardRefusesToAcceptAResolvedFinding(t *testing.T) {
	m, st := guardHarness(t)
	m.scan(map[string]any{})
	f := ruleSet(m.findings("").Findings)["sg-open-all|sg-all"]
	st.mu.Lock()
	st.noWorld = true
	st.mu.Unlock()
	m.scan(map[string]any{})
	m.admin.expect(409, "POST", "/v1/guard/findings/"+f.ID+"/accept", map[string]any{"reason": "A perfectly good reason here", "days": 7})
}

func TestGuardScansEveryAccountAndAccountsAreIndependent(t *testing.T) {
	m, st := guardHarness(t)
	m.admin.expect(201, "POST", "/v1/aws/accounts", map[string]any{"name": "Staging", "profile": "staging", "region": "eu-west-1"})
	sc := m.scan(map[string]any{})
	if len(sc.Accounts) != 2 || sc.Accounts[0].Findings == 0 || sc.Accounts[1].Findings == 0 {
		t.Fatalf("scan = %+v", sc.Accounts)
	}
	byAccount := map[string]int{}
	for _, f := range m.findings("").Findings {
		byAccount[f.Account]++
	}
	if byAccount["primary"] == 0 || byAccount["staging"] == 0 || byAccount["primary"] != byAccount["staging"] {
		t.Errorf("findings by account = %v", byAccount)
	}
	// The same resource id in two accounts is two findings.
	ids := map[string]bool{}
	for _, f := range m.findings("").Findings {
		if ids[f.ID] {
			t.Fatalf("finding id %s repeats across accounts", f.ID)
		}
		ids[f.ID] = true
	}
	// Scanning one account leaves the other's findings alone.
	st.mu.Lock()
	st.noWorld = true
	st.mu.Unlock()
	m.scan(map[string]any{"account": "staging"})
	if len(m.findings("?account=primary").Findings) != byAccount["primary"] {
		t.Error("scanning staging changed the primary account's findings")
	}
	if _, ok := ruleSet(m.findings("?account=staging").Findings)["sg-open-all|sg-all"]; ok {
		t.Error("staging's fixed group is still open")
	}
}

func TestGuardOneScanAtATimeAndNeedsAnAccount(t *testing.T) {
	m, st := guardHarness(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock) // a failing test must not leave a scan blocked forever
	st.mu.Lock()
	st.block = release
	st.mu.Unlock()
	var sc GuardScan
	json.Unmarshal(m.dev.expect(202, "POST", "/v1/guard/scan", map[string]any{}), &sc)
	m.dev.expect(409, "POST", "/v1/guard/scan", map[string]any{})
	unblock()
	m.waitFor("the scan", func() bool {
		var out struct{ Scans []GuardScan }
		json.Unmarshal(m.v.expect(200, "GET", "/v1/guard/scans", nil), &out)
		return len(out.Scans) > 0 && out.Scans[0].Status != "running"
	})
	m.v.expect(403, "POST", "/v1/guard/scan", map[string]any{})
	// No account connected at all.
	m.admin.expect(200, "PUT", "/v1/integrations/aws", map[string]any{"enabled": false, "profile": "", "region": ""})
	m.dev.expect(422, "POST", "/v1/guard/scan", map[string]any{})
}

func TestGuardAutomaticScansAndSettings(t *testing.T) {
	m, _ := guardHarness(t)
	m.dev.expect(403, "PUT", "/v1/guard/settings", map[string]any{"every_hours": 24})
	m.admin.expect(400, "PUT", "/v1/guard/settings", map[string]any{"every_hours": 5})
	ctx := context.Background()
	t0 := time.Now()
	m.srv.guardTick(ctx, t0)
	var out struct {
		Scans      []GuardScan
		EveryHours int `json:"every_hours"`
	}
	json.Unmarshal(m.v.expect(200, "GET", "/v1/guard/scans", nil), &out)
	if len(out.Scans) != 0 {
		t.Fatal("a scan ran with automatic scans off")
	}
	m.admin.expect(200, "PUT", "/v1/guard/settings", map[string]any{"every_hours": 6})
	m.srv.guardTick(ctx, t0)
	m.waitFor("the automatic scan", func() bool {
		json.Unmarshal(m.v.expect(200, "GET", "/v1/guard/scans", nil), &out)
		return len(out.Scans) == 1 && out.Scans[0].Status == "done"
	})
	if out.Scans[0].Trigger != "schedule" || out.EveryHours != 6 {
		t.Errorf("scan = %+v, every %d", out.Scans[0], out.EveryHours)
	}
	m.srv.guardTick(ctx, t0.Add(3*time.Hour))
	time.Sleep(50 * time.Millisecond)
	json.Unmarshal(m.v.expect(200, "GET", "/v1/guard/scans", nil), &out)
	if len(out.Scans) != 1 {
		t.Error("scanned again inside the interval")
	}
	m.srv.guardTick(ctx, t0.Add(7*time.Hour))
	m.waitFor("the second automatic scan", func() bool {
		json.Unmarshal(m.v.expect(200, "GET", "/v1/guard/scans", nil), &out)
		return len(out.Scans) == 2 && out.Scans[0].Status == "done"
	})
}

func TestGuardOnlyReads(t *testing.T) {
	m, st := guardHarness(t)
	m.scan(map[string]any{})
	for _, c := range st.calls {
		f := strings.Fields(strings.SplitN(c, ": ", 2)[1])
		if len(f) < 2 {
			continue
		}
		op := f[1]
		if !(strings.HasPrefix(op, "list-") || strings.HasPrefix(op, "describe-") || strings.HasPrefix(op, "get-") || op == "generate-credential-report") {
			t.Errorf("the guard ran %q, which is not a read", c)
		}
	}
}

func TestGuardRestartFailsAScanThatWasRunning(t *testing.T) {
	m, _ := guardHarness(t)
	m.srv.docs.Put(context.Background(), kindGuardScan, "cut", GuardScan{ID: "cut", Status: "running", StartedAt: time.Now()})
	m.srv.failInterruptedScans(context.Background())
	var s GuardScan
	m.srv.docs.Get(context.Background(), kindGuardScan, "cut", &s)
	if s.Status != "failed" || !strings.Contains(s.Error, "restarted") || s.FinishedAt == nil {
		t.Errorf("scan = %+v", s)
	}
}

func TestGuardLookupForTheAssistantAndAgents(t *testing.T) {
	m, _ := guardHarness(t)
	m.ask(m.dev, lookupReply("guard_findings", map[string]any{}))
	if res := m.llm.lastUser(1); !strings.Contains(res, "No open findings") {
		t.Errorf("before any scan: %s", res)
	}
	m.scan(map[string]any{})
	f := ruleSet(m.findings("").Findings)["sg-open-ports|sg-ssh"]
	m.admin.expect(200, "POST", "/v1/guard/findings/"+f.ID+"/accept", map[string]any{"reason": "Office VPN range is being added next sprint", "days": 14})
	m.ask(m.dev, lookupReply("guard_findings", map[string]any{}))
	res := m.llm.lastUser(1)
	if !strings.Contains(res, "[critical] Security group allows all traffic") || !strings.Contains(res, "Fix:") || strings.Contains(res, "ssh-open") || !strings.Contains(res, "1 accepted") {
		t.Errorf("lookup = %s", res)
	}
	if strings.Index(res, "[critical]") > strings.Index(res, "[low]") {
		t.Error("not worst first")
	}
	m.ask(m.dev, lookupReply("guard_findings", map[string]any{"severity": "info"}))
	if res := m.llm.lastUser(1); !strings.Contains(res, "ecs-idle-cluster") && !strings.Contains(res, "no services") {
		t.Errorf("severity filter: %s", res)
	}
	// An agent can be given the tool.
	a := newAgent(t, m.admin, agentBody("Guard reader", nil, []string{"guard_findings"}, "T0"))
	m.llm.script(lookupReply("guard_findings", map[string]any{"severity": "critical"}), "done")
	m.dev.expect(200, "POST", "/v1/ai/agents/"+a.ID+"/chat", map[string]any{"message": "what is critical?"})
	if !strings.Contains(m.llm.lastUser(1), "wide-open") {
		t.Errorf("agent lookup: %s", m.llm.lastUser(1))
	}
}

func TestCredentialReportFindingsReadsPlainAndEncodedReports(t *testing.T) {
	g := &guardStub{}
	now := time.Now().UTC()
	enc := g.credentialReport(now)
	plain, _ := base64.StdEncoding.DecodeString(enc)
	for name, content := range map[string]string{"base64": enc, "plain": string(plain)} {
		fs, err := credentialReportFindings(content, now)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rules := map[string]bool{}
		for _, f := range fs {
			rules[f.Rule+"|"+f.Resource] = true
		}
		for _, want := range []string{"iam-root-keys|root", "iam-root-no-mfa|root", "iam-user-no-mfa|alice", "iam-old-access-key|bob/key1"} {
			if !rules[want] {
				t.Errorf("%s: missing %s in %v", name, want, rules)
			}
		}
		if len(fs) != 4 {
			t.Errorf("%s: %d findings, want 4 (carol is clean)", name, len(fs))
		}
	}
	if _, err := credentialReportFindings("not a report", now); err == nil {
		t.Error("a bad report should be an error, not no findings")
	}
}

func TestSecurityGroupExposureIsReadFromTheRules(t *testing.T) {
	item := map[string]any{"IpPermissions": []any{
		map[string]any{"IpProtocol": "tcp", "FromPort": 22.0, "ToPort": 22.0, "IpRanges": []any{map[string]any{"CidrIp": "0.0.0.0/0"}}},
		map[string]any{"IpProtocol": "udp", "FromPort": 5000.0, "ToPort": 5100.0, "Ipv6Ranges": []any{map[string]any{"CidrIpv6": "::/0"}}},
		map[string]any{"IpProtocol": "tcp", "FromPort": 3306.0, "ToPort": 3306.0, "IpRanges": []any{map[string]any{"CidrIp": "10.0.0.0/8"}}},
	}}
	f := openToInternet(item)
	if len(f) != 1 || f[0].Value != "tcp 22; udp 5000-5100" {
		t.Errorf("exposure = %+v", f)
	}
	if openToInternet(map[string]any{}) != nil {
		t.Error("a group with no rules is not exposed")
	}
}

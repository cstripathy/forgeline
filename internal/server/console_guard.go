package server

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/forgelinehq/forgeline/internal/docstore"
)

// Guard (note 017, slice 4): deterministic security and hygiene checks over every connected AWS
// account. Rules run on what the explorer already reads, plus S3 public-access settings and the IAM
// credential report. Findings keep a stable id, first and last seen, and resolve themselves when the
// cause is gone. An administrator can accept a finding for a limited time with a reason. Scans read
// only; nothing here changes AWS. Agents and the assistant read the findings (`guard_findings`).

const (
	kindGuardFinding = "guardfinding"
	kindGuardExc     = "guardexception"
	kindGuardScan    = "guardscan"
	kindGuardState   = "guardstate"

	maxExceptionDays = 90
	maxGuardScans    = 20
	maxS3Checked     = 40
	guardTimeout     = 4 * time.Minute

	sevCritical = "critical"
	sevHigh     = "high"
	sevMedium   = "medium"
	sevLow      = "low"
	sevInfo     = "info"
)

var severityRank = map[string]int{sevCritical: 5, sevHigh: 4, sevMedium: 3, sevLow: 2, sevInfo: 1}

// GuardFinding is one problem found in one resource.
type GuardFinding struct {
	ID          string     `json:"id"`
	Rule        string     `json:"rule"`
	Title       string     `json:"title"`
	Severity    string     `json:"severity"`
	Account     string     `json:"account"` // the console's account id ("primary" or an added one)
	AccountName string     `json:"account_name"`
	AWSAccount  string     `json:"aws_account,omitempty"`
	Region      string     `json:"region,omitempty"`
	Service     string     `json:"service"`
	Resource    string     `json:"resource"`
	Name        string     `json:"name"`
	Detail      string     `json:"detail"`
	Fix         string     `json:"fix"`
	Status      string     `json:"status"` // open or resolved
	FirstSeen   time.Time  `json:"first_seen"`
	LastSeen    time.Time  `json:"last_seen"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
}

// GuardException accepts a finding until it expires.
type GuardException struct {
	ID      string    `json:"id"` // the finding id
	Reason  string    `json:"reason"`
	By      string    `json:"by"`
	At      time.Time `json:"at"`
	Expires time.Time `json:"expires"`
}

// GuardFindingView is a finding with its exception, if one is in force.
type GuardFindingView struct {
	GuardFinding
	Accepted *GuardException `json:"accepted,omitempty"`
}

// GuardScanAccount is what a scan did for one account.
type GuardScanAccount struct {
	Account  string   `json:"account"`
	Name     string   `json:"name"`
	Findings int      `json:"findings"`
	Error    string   `json:"error,omitempty"`
	Skipped  []string `json:"skipped,omitempty"` // checks that could not run (usually permissions)
}

// GuardScan is one pass over the accounts.
type GuardScan struct {
	ID         string             `json:"id"`
	Status     string             `json:"status"` // running, done or failed
	Trigger    string             `json:"trigger"`
	By         string             `json:"by"`
	StartedAt  time.Time          `json:"started_at"`
	FinishedAt *time.Time         `json:"finished_at,omitempty"`
	Accounts   []GuardScanAccount `json:"accounts"`
	Error      string             `json:"error,omitempty"`
}

// GuardSettings is how often scans run by themselves.
type GuardSettings struct {
	EveryHours int       `json:"every_hours"` // 0 is off
	LastAuto   time.Time `json:"last_auto,omitempty"`
}

type rawFinding struct {
	Rule, Title, Severity, Service, Resource, Name, Detail, Fix string
}

type guardEngine struct {
	mu      sync.Mutex
	running bool
}

// --- rules over the inventory ---

func fieldOf(r AWSResourceJSON, label string) string {
	for _, f := range r.Fields {
		if f.Label == label {
			return f.Value
		}
	}
	return ""
}

func svcOf(inv AWSInventoryJSON, id string) AWSServiceJSON {
	for _, s := range inv.Services {
		if s.ID == id {
			return s
		}
	}
	return AWSServiceJSON{}
}

var deprecatedRuntimes = map[string]bool{"python2.7": true, "python3.6": true, "python3.7": true, "python3.8": true, "nodejs10.x": true, "nodejs12.x": true, "nodejs14.x": true,
	"nodejs16.x": true, "java8": true, "dotnetcore2.1": true, "dotnetcore3.1": true, "dotnet5.0": true, "dotnet6": true, "ruby2.5": true, "ruby2.7": true, "go1.x": true, "provided": true}

// inventoryRules is every check that needs nothing beyond the explorer's data.
func inventoryRules(inv AWSInventoryJSON, now time.Time) []rawFinding {
	var out []rawFinding
	add := func(f rawFinding) { out = append(out, f) }

	for _, r := range svcOf(inv, "sg").Resources {
		open := fieldOf(r, "Open to the internet")
		if open == "" {
			continue
		}
		all := false
		var risky []string
		for _, p := range strings.Split(open, "; ") {
			switch {
			case p == "all traffic":
				all = true
			case p == "tcp 80" || p == "tcp 443":
			default:
				risky = append(risky, p)
			}
		}
		switch {
		case all:
			add(rawFinding{"sg-open-all", "Security group allows all traffic from the internet", sevCritical, "sg", r.ID, r.Name, "Open to the internet: " + open,
				"Replace the rule with the specific ports and sources that are needed. Anything on the instances behind it is reachable by anyone."})
		case len(risky) > 0:
			add(rawFinding{"sg-open-ports", "Security group opens non-web ports to the internet", sevHigh, "sg", r.ID, r.Name, "Open to the internet: " + open,
				"Limit these rules to known addresses, a VPN or a bastion, or use Session Manager instead of SSH or RDP."})
		}
	}
	for _, r := range svcOf(inv, "ebs").Resources {
		if fieldOf(r, "Encrypted") == "false" {
			add(rawFinding{"ebs-unencrypted", "EBS volume is not encrypted", sevMedium, "ebs", r.ID, r.Name, "Size " + fieldOf(r, "Size (GiB)") + " GiB, " + fieldOf(r, "Type"),
				"Snapshot it, copy the snapshot with encryption on, and replace the volume. Turn on default EBS encryption for the region so new volumes are encrypted."})
		}
	}
	for _, r := range svcOf(inv, "rds").Resources {
		if fieldOf(r, "Publicly accessible") == "true" {
			add(rawFinding{"rds-public", "Database is publicly accessible", sevHigh, "rds", r.ID, r.Name, "Engine " + fieldOf(r, "Engine"),
				"Turn off public accessibility and reach it from inside the VPC, through a bastion or a VPN."})
		}
		if fieldOf(r, "Encrypted") == "false" {
			add(rawFinding{"rds-unencrypted", "Database storage is not encrypted", sevMedium, "rds", r.ID, r.Name, "Engine " + fieldOf(r, "Engine"),
				"Restore a snapshot copy with encryption on and switch the application to it."})
		}
	}
	for _, r := range svcOf(inv, "cfn").Resources {
		if strings.Contains(r.State, "ROLLBACK") || strings.HasSuffix(r.State, "_FAILED") {
			add(rawFinding{"cfn-failed-stack", "CloudFormation stack is in a failed state", sevLow, "cfn", r.ID, r.Name, r.State,
				"Find the failed resource in the stack events. A stack stuck in ROLLBACK_COMPLETE must be deleted before it can be deployed again; its leftovers can cost money."})
		}
	}
	for _, r := range svcOf(inv, "ecs").Resources {
		if r.State == "ACTIVE" && fieldOf(r, "Services") == "0" && fieldOf(r, "Running tasks") == "0" && fieldOf(r, "Pending tasks") == "0" && fieldOf(r, "Container instances") == "0" {
			add(rawFinding{"ecs-idle-cluster", "ECS cluster has no services and no tasks", sevInfo, "ecs", r.ID, r.Name, "Nothing runs in it",
				"Delete it if it is not needed; an empty cluster costs nothing but clutters the account."})
		}
	}
	for _, r := range svcOf(inv, "ec2").Resources {
		if r.State == "stopped" {
			age := ""
			if t, err := time.Parse(time.RFC3339, r.Created); err == nil && now.Sub(t) > 30*24*time.Hour {
				age = fmt.Sprintf(", launched %d days ago", int(now.Sub(t).Hours()/24))
			}
			add(rawFinding{"ec2-stopped", "Stopped instance still has storage billing", sevLow, "ec2", r.ID, r.Name, "Type " + fieldOf(r, "Type") + age,
				"Terminate it if it is no longer needed (snapshot the volume first if the data matters), or start it if it should be running."})
		}
	}
	for _, r := range svcOf(inv, "logs").Resources {
		if fieldOf(r, "Retention (days)") == "" {
			add(rawFinding{"logs-no-retention", "Log group keeps logs forever", sevLow, "logs", r.ID, r.Name, "No retention period is set",
				"Set a retention period, such as 30 or 90 days, so the stored volume stops growing."})
		}
	}
	for _, r := range svcOf(inv, "ecr").Resources {
		if fieldOf(r, "Scan on push") == "false" {
			add(rawFinding{"ecr-no-scan", "Image repository does not scan on push", sevLow, "ecr", r.ID, r.Name, "Scan on push is off",
				"Turn on scan on push, or enable enhanced scanning for the registry, so known vulnerabilities in pushed images are reported."})
		}
	}
	for _, r := range svcOf(inv, "lambda").Resources {
		if rt := fieldOf(r, "Runtime"); deprecatedRuntimes[rt] {
			add(rawFinding{"lambda-old-runtime", "Lambda function uses a runtime that is no longer supported", sevMedium, "lambda", r.ID, r.Name, "Runtime " + rt,
				"Move the function to a supported runtime. Unsupported runtimes no longer get security patches."})
		}
	}
	return out
}

// --- the extra reads: S3 public access and the IAM credential report ---

// s3Findings reads each bucket's public-access block. A bucket with none, or with parts turned off,
// can be made public by one wrong policy or ACL.
func (s *Server) s3Findings(ctx context.Context, st awsSettings, inv AWSInventoryJSON) ([]rawFinding, error) {
	var out []rawFinding
	buckets := svcOf(inv, "s3").Resources
	if len(buckets) > maxS3Checked {
		buckets = buckets[:maxS3Checked]
	}
	var denied error
	for _, b := range buckets {
		raw, err := s.aws(ctx, st, "s3api", "get-public-access-block", "--bucket", b.ID)
		if err != nil {
			low := strings.ToLower(err.Error())
			switch {
			case strings.Contains(low, "nosuchpublicaccessblock"):
				out = append(out, rawFinding{"s3-no-public-block", "S3 bucket has no public access block", sevHigh, "s3-access", b.ID, b.Name, "No block public access settings at all",
					"Turn on all four Block Public Access settings for the bucket, and for the whole account unless a bucket must be public."})
			case strings.Contains(low, "denied") || strings.Contains(low, "not authorized"):
				denied = err
			}
			continue
		}
		var cfg struct {
			PublicAccessBlockConfiguration map[string]bool
		}
		_ = json.Unmarshal(raw, &cfg)
		var off []string
		for _, k := range []string{"BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets"} {
			if !cfg.PublicAccessBlockConfiguration[k] {
				off = append(off, k)
			}
		}
		if len(off) > 0 {
			out = append(out, rawFinding{"s3-partial-public-block", "S3 bucket public access block is only partly on", sevMedium, "s3-access", b.ID, b.Name, "Off: " + strings.Join(off, ", "),
				"Turn on all four Block Public Access settings unless the bucket is meant to be public."})
		}
	}
	if len(out) == 0 && denied != nil {
		return nil, denied
	}
	return out, nil
}

// iamFindings reads the credential report: who has a password or keys, and whether MFA and key age
// are in order. The report holds no secrets.
func (s *Server) iamFindings(ctx context.Context, st awsSettings, now time.Time) ([]rawFinding, error) {
	if _, err := s.aws(ctx, st, "iam", "generate-credential-report"); err != nil {
		return nil, err
	}
	var content string
	for i := 0; i < 5; i++ { // generation is usually instant, but allow a moment
		raw, err := s.aws(ctx, st, "iam", "get-credential-report")
		if err == nil {
			var r struct{ Content string }
			if json.Unmarshal(raw, &r) == nil && r.Content != "" {
				content = r.Content
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if content == "" {
		return nil, errors.New("the credential report was not ready")
	}
	return credentialReportFindings(content, now)
}

func credentialReportFindings(content string, now time.Time) ([]rawFinding, error) {
	text := content
	if dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(content)); err == nil && strings.Contains(string(dec), "user,arn") {
		text = string(dec)
	}
	rows, err := csv.NewReader(strings.NewReader(text)).ReadAll()
	if err != nil || len(rows) < 2 {
		return nil, errors.New("could not read the credential report")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	get := func(row []string, k string) string {
		if i, ok := col[k]; ok && i < len(row) {
			return row[i]
		}
		return ""
	}
	var out []rawFinding
	for _, row := range rows[1:] {
		user := get(row, "user")
		if user == "<root_account>" {
			if get(row, "access_key_1_active") == "true" || get(row, "access_key_2_active") == "true" {
				out = append(out, rawFinding{"iam-root-keys", "The root user has an access key", sevCritical, "iam", "root", "root account", "An active access key exists for the root user",
					"Delete the root access keys and use roles or named users. Root keys cannot be limited."})
			}
			if get(row, "mfa_active") != "true" {
				out = append(out, rawFinding{"iam-root-no-mfa", "The root user has no MFA", sevHigh, "iam", "root", "root account", "MFA is not active for the root user",
					"Turn on a hardware or authenticator MFA for the root user."})
			}
			continue
		}
		if get(row, "password_enabled") == "true" && get(row, "mfa_active") != "true" {
			out = append(out, rawFinding{"iam-user-no-mfa", "A user can sign in with a password and has no MFA", sevHigh, "iam", user, user, "Console password is enabled and MFA is not active",
				"Require MFA for the user, or move people to single sign-on and remove the IAM passwords."})
		}
		for _, n := range []string{"1", "2"} {
			if get(row, "access_key_"+n+"_active") != "true" {
				continue
			}
			rot, err := time.Parse(time.RFC3339, get(row, "access_key_"+n+"_last_rotated"))
			if err == nil && now.Sub(rot) > 90*24*time.Hour {
				out = append(out, rawFinding{"iam-old-access-key", "An access key has not been rotated for over 90 days", sevMedium, "iam", user + "/key" + n, user,
					fmt.Sprintf("Key %s is %d days old", n, int(now.Sub(rot).Hours()/24)), "Rotate the key or replace it with a role. Long-lived keys that leak stay valid until someone notices."})
			}
		}
	}
	return out, nil
}

// --- scanning and reconciling ---

func findingID(account, rule, resource string) string {
	return sha("guard", account, rule, resource)[:16]
}

// scanAccount runs every check for one account and reconciles its findings with what is stored.
func (s *Server) scanAccount(ctx context.Context, a AWSAccountJSON, now time.Time) GuardScanAccount {
	res := GuardScanAccount{Account: a.ID, Name: a.Name}
	st, err := s.awsFor(ctx, a.ID)
	if err != nil || !st.Enabled {
		res.Error = "the account is not connected"
		return res
	}
	inv := s.inventoryFor(ctx, a.ID, st, true)
	if inv.Error != "" {
		res.Error = inv.Error
		return res
	}
	var raws []rawFinding
	ok := map[string]bool{} // services whose state we know, so a failed read never resolves anything
	for _, svc := range inv.Services {
		if svc.Error == "" {
			ok[svc.ID] = true
		} else {
			res.Skipped = append(res.Skipped, svc.Label)
		}
	}
	raws = append(raws, inventoryRules(inv, now)...)
	if svcOf(inv, "s3").Error == "" {
		if f, err := s.s3Findings(ctx, st, inv); err != nil {
			res.Skipped = append(res.Skipped, "S3 public access ("+inventoryError(err)+")")
		} else {
			raws = append(raws, f...)
			ok["s3-access"] = true // its own key: listing the buckets working says nothing about reading their settings
		}
	}
	if f, err := s.iamFindings(ctx, st, now); err != nil {
		res.Skipped = append(res.Skipped, "IAM credentials ("+inventoryError(err)+")")
	} else {
		raws = append(raws, f...)
		ok["iam"] = true
	}
	res.Findings = s.reconcile(ctx, a.ID, a.Name, inv.Account, inv.Region, raws, ok, now)
	return res
}

func (s *Server) reconcile(ctx context.Context, account, accountName, awsAccount, region string, raws []rawFinding, known map[string]bool, now time.Time) int {
	seen := map[string]bool{}
	for _, r := range raws {
		id := findingID(account, r.Rule, r.Resource)
		seen[id] = true
		f := GuardFinding{ID: id, Rule: r.Rule, Title: r.Title, Severity: r.Severity, Account: account, AccountName: accountName, AWSAccount: awsAccount, Region: region,
			Service: r.Service, Resource: r.Resource, Name: r.Name, Detail: r.Detail, Fix: r.Fix, Status: "open", FirstSeen: now, LastSeen: now}
		var old GuardFinding
		if err := s.docs.Get(ctx, kindGuardFinding, id, &old); err == nil {
			f.FirstSeen = old.FirstSeen // a finding that comes back keeps its history; resolved ones reopen
		}
		_ = s.docs.Put(ctx, kindGuardFinding, id, f)
	}
	docs, err := s.docs.List(ctx, kindGuardFinding, docstore.ListOptions{})
	if err != nil {
		return len(seen)
	}
	old, _ := docstore.Decode[GuardFinding](docs)
	for _, f := range old {
		if f.Account != account || f.Status != "open" || seen[f.ID] || !known[f.Service] {
			continue
		}
		t := now
		f.Status, f.ResolvedAt = "resolved", &t
		_ = s.docs.Put(ctx, kindGuardFinding, f.ID, f)
	}
	return len(seen)
}

// startScan records a scan and runs it in the background. Only one runs at a time.
func (s *Server) startScan(ctx context.Context, trigger string, by Principal, only string) (GuardScan, error) {
	s.guard.mu.Lock()
	if s.guard.running {
		s.guard.mu.Unlock()
		return GuardScan{}, errors.New("a scan is already running")
	}
	accts, err := s.accountViews(ctx)
	if err != nil {
		s.guard.mu.Unlock()
		return GuardScan{}, err
	}
	if only != "" {
		var keep []AWSAccountJSON
		for _, a := range accts {
			if a.ID == only {
				keep = append(keep, a)
			}
		}
		accts = keep
	}
	var clusters []K8sCluster
	if cs, err := s.listK8sClusters(ctx); err == nil {
		for _, c := range cs {
			if only == "" || only == "k8s:"+c.ID {
				clusters = append(clusters, c)
			}
		}
	}
	pipelines := only == "" || only == pipelineScanID
	if len(accts) == 0 && len(clusters) == 0 && !pipelines {
		s.guard.mu.Unlock()
		return GuardScan{}, errors.New("there is no AWS account or cluster to scan; connect one first")
	}
	s.guard.running = true
	scan := GuardScan{ID: randomID(), Status: "running", Trigger: trigger, By: by.Name, StartedAt: time.Now().UTC(), Accounts: []GuardScanAccount{}}
	_ = s.docs.Put(ctx, kindGuardScan, scan.ID, scan)
	s.guard.mu.Unlock()

	base := s.baseCtx
	if base == nil {
		base = context.Background()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { s.guard.mu.Lock(); s.guard.running = false; s.guard.mu.Unlock() }()
		cctx, cancel := context.WithTimeout(base, guardTimeout)
		defer cancel()
		for _, a := range accts {
			scan.Accounts = append(scan.Accounts, s.scanAccount(cctx, a, time.Now().UTC()))
			_ = s.docs.Put(context.Background(), kindGuardScan, scan.ID, scan)
		}
		for _, c := range clusters {
			scan.Accounts = append(scan.Accounts, s.scanCluster(cctx, c, time.Now().UTC()))
			_ = s.docs.Put(context.Background(), kindGuardScan, scan.ID, scan)
		}
		if pipelines {
			scan.Accounts = append(scan.Accounts, s.scanPipelines(cctx, time.Now().UTC()))
			_ = s.docs.Put(context.Background(), kindGuardScan, scan.ID, scan)
		}
		now := time.Now().UTC()
		scan.Status, scan.FinishedAt = "done", &now
		failed := 0
		for _, a := range scan.Accounts {
			if a.Error != "" {
				failed++
			}
		}
		if failed == len(scan.Accounts) {
			scan.Status, scan.Error = "failed", "no account could be scanned"
		}
		_ = s.docs.Put(context.Background(), kindGuardScan, scan.ID, scan)
		s.pruneScans(context.Background())
		_ = s.audit(context.Background(), by, "", "guard.scan", "guard", fmt.Sprintf("%s: %d accounts, %s", trigger, len(scan.Accounts), scan.Status), scan.Status == "done")
	}()
	return scan, nil
}

func (s *Server) pruneScans(ctx context.Context) {
	docs, err := s.docs.List(ctx, kindGuardScan, docstore.ListOptions{Desc: true, Limit: maxGuardScans + 20})
	if err != nil || len(docs) <= maxGuardScans {
		return
	}
	scans, _ := docstore.Decode[GuardScan](docs)
	for _, sc := range scans[maxGuardScans:] {
		_ = s.docs.Delete(ctx, kindGuardScan, sc.ID)
	}
}

// failInterruptedScans marks scans a restart cut short.
func (s *Server) failInterruptedScans(ctx context.Context) {
	docs, _ := s.docs.List(ctx, kindGuardScan, docstore.ListOptions{Limit: maxGuardScans * 2})
	scans, _ := docstore.Decode[GuardScan](docs)
	for _, sc := range scans {
		if sc.Status == "running" {
			now := time.Now().UTC()
			sc.Status, sc.Error, sc.FinishedAt = "failed", "server restarted", &now
			_ = s.docs.Put(ctx, kindGuardScan, sc.ID, sc)
		}
	}
}

// guardTick starts the automatic scan when its interval has passed.
func (s *Server) guardTick(ctx context.Context, now time.Time) {
	var gs GuardSettings
	_ = s.docs.Get(ctx, kindGuardState, "settings", &gs)
	if gs.EveryHours == 0 || now.Sub(gs.LastAuto) < time.Duration(gs.EveryHours)*time.Hour {
		return
	}
	if _, err := s.startScan(ctx, "schedule", Principal{Name: "guard", Role: "viewer"}, ""); err != nil {
		return
	}
	gs.LastAuto = now
	_ = s.docs.Put(ctx, kindGuardState, "settings", gs)
}

// --- handlers ---

func (s *Server) loadExceptions(ctx context.Context, now time.Time) map[string]GuardException {
	out := map[string]GuardException{}
	docs, err := s.docs.List(ctx, kindGuardExc, docstore.ListOptions{})
	if err != nil {
		return out
	}
	exc, _ := docstore.Decode[GuardException](docs)
	for _, e := range exc {
		if e.Expires.After(now) { // an expired acceptance no longer counts, so the finding reopens
			out[e.ID] = e
		}
	}
	return out
}

func (s *Server) guardViews(ctx context.Context, now time.Time) ([]GuardFindingView, error) {
	docs, err := s.docs.List(ctx, kindGuardFinding, docstore.ListOptions{})
	if err != nil {
		return nil, err
	}
	fs, err := docstore.Decode[GuardFinding](docs)
	if err != nil {
		return nil, err
	}
	exc := s.loadExceptions(ctx, now)
	out := make([]GuardFindingView, 0, len(fs))
	for _, f := range fs {
		v := GuardFindingView{GuardFinding: f}
		if e, ok := exc[f.ID]; ok {
			e := e
			v.Accepted = &e
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Accepted != nil) != (b.Accepted != nil) {
			return a.Accepted == nil
		}
		if severityRank[a.Severity] != severityRank[b.Severity] {
			return severityRank[a.Severity] > severityRank[b.Severity]
		}
		return a.Name < b.Name
	})
	return out, nil
}

func (s *Server) handleGuardFindings(w http.ResponseWriter, r *http.Request) {
	all, err := s.guardViews(r.Context(), time.Now())
	if err != nil {
		s.internalError(w, "reading the findings", err)
		return
	}
	q := r.URL.Query()
	status := q.Get("status")
	if status == "" {
		status = "open"
	}
	counts := map[string]int{}
	accepted := 0
	out := []GuardFindingView{}
	for _, f := range all {
		if q.Get("account") != "" && f.Account != q.Get("account") {
			continue
		}
		if f.Status != status {
			continue
		}
		if f.Accepted != nil {
			accepted++
		} else if status == "open" {
			counts[f.Severity]++
		}
		if q.Get("severity") != "" && f.Severity != q.Get("severity") {
			continue
		}
		out = append(out, f)
	}
	writeJSON(w, http.StatusOK, map[string]any{"findings": out, "counts": counts, "accepted": accepted})
}

func (s *Server) handleGuardScans(w http.ResponseWriter, r *http.Request) {
	docs, err := s.docs.List(r.Context(), kindGuardScan, docstore.ListOptions{Desc: true, Limit: maxGuardScans})
	if err != nil {
		s.internalError(w, "reading the scans", err)
		return
	}
	scans, _ := docstore.Decode[GuardScan](docs)
	if scans == nil {
		scans = []GuardScan{}
	}
	var gs GuardSettings
	_ = s.docs.Get(r.Context(), kindGuardState, "settings", &gs)
	writeJSON(w, http.StatusOK, map[string]any{"scans": scans, "every_hours": gs.EveryHours})
}

func (s *Server) handleGuardScan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account string `json:"account"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	scan, err := s.startScan(r.Context(), "manual", principalFrom(r.Context()), req.Account)
	if err != nil {
		code := http.StatusConflict
		if strings.Contains(err.Error(), "no AWS account or cluster") {
			code = http.StatusUnprocessableEntity
		}
		writeError(w, code, err.Error())
		return
	}
	s.auditReq(r, "guard.scan.start", "guard", "scan "+scan.ID, true)
	writeJSON(w, http.StatusAccepted, scan)
}

func (s *Server) handleGuardSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EveryHours int `json:"every_hours"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	switch req.EveryHours {
	case 0, 6, 12, 24, 168:
	default:
		writeError(w, http.StatusBadRequest, "scan every 6, 12, 24 or 168 hours, or 0 to turn automatic scans off")
		return
	}
	var gs GuardSettings
	_ = s.docs.Get(r.Context(), kindGuardState, "settings", &gs)
	gs.EveryHours = req.EveryHours
	if err := s.docs.Put(r.Context(), kindGuardState, "settings", gs); err != nil {
		s.internalError(w, "saving the settings", err)
		return
	}
	s.auditReq(r, "guard.settings", "guard", "every "+strconv.Itoa(req.EveryHours)+" hours", true)
	writeJSON(w, http.StatusOK, gs)
}

func (s *Server) handleGuardAccept(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
		Days   int    `json:"days"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	switch {
	case len(req.Reason) < 10 || len(req.Reason) > 500:
		writeError(w, http.StatusBadRequest, "say why this is acceptable (10 to 500 characters); it is shown to everyone and kept in the audit log")
		return
	case req.Days < 1 || req.Days > maxExceptionDays:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("accept it for 1 to %d days; after that it is reviewed again", maxExceptionDays))
		return
	}
	var f GuardFinding
	if err := s.docs.Get(r.Context(), kindGuardFinding, r.PathValue("id"), &f); errors.Is(err, docstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such finding")
		return
	} else if err != nil {
		s.internalError(w, "reading the finding", err)
		return
	}
	if f.Status != "open" {
		writeError(w, http.StatusConflict, "this finding is already resolved")
		return
	}
	now := time.Now().UTC()
	e := GuardException{ID: f.ID, Reason: req.Reason, By: principalFrom(r.Context()).Name, At: now, Expires: now.Add(time.Duration(req.Days) * 24 * time.Hour)}
	if err := s.docs.Put(r.Context(), kindGuardExc, f.ID, e); err != nil {
		s.internalError(w, "saving the exception", err)
		return
	}
	s.auditReq(r, "guard.accept", "guard/"+f.ID, fmt.Sprintf("%s %s in %s for %d days: %s", f.Rule, f.Name, f.AccountName, req.Days, req.Reason), true)
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleGuardUnaccept(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var e GuardException
	if err := s.docs.Get(r.Context(), kindGuardExc, id, &e); errors.Is(err, docstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "that finding is not accepted")
		return
	}
	if err := s.docs.Delete(r.Context(), kindGuardExc, id); err != nil {
		s.internalError(w, "removing the exception", err)
		return
	}
	s.auditReq(r, "guard.unaccept", "guard/"+id, "", true)
	w.WriteHeader(http.StatusNoContent)
}

// lookupGuardFindings gives the assistant and agents the open findings, worst first.
func (s *Server) lookupGuardFindings(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Account  string `json:"account"`
		Severity string `json:"severity"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	all, err := s.guardViews(ctx, time.Now())
	if err != nil {
		return "", errors.New("could not read the findings")
	}
	var b strings.Builder
	n, accepted := 0, 0
	for _, f := range all {
		if f.Status != "open" || (a.Account != "" && f.Account != a.Account) || (a.Severity != "" && f.Severity != a.Severity) {
			continue
		}
		if f.Accepted != nil {
			accepted++
			continue
		}
		n++
		if n <= 60 {
			fmt.Fprintf(&b, "- [%s] %s: %s (%s %s, account %s) %s. Fix: %s\n", f.Severity, f.Title, f.Name, f.Service, f.Resource, f.AccountName, f.Detail, f.Fix)
		}
	}
	if n == 0 {
		return fmt.Sprintf("No open findings (%d accepted). If no scan has run, start one on the Guard page.", accepted), nil
	}
	head := fmt.Sprintf("%d open findings, worst first (%d accepted by an administrator are left out)", n, accepted)
	if n > 60 {
		head += "; showing the first 60"
	}
	return head + ":\n" + b.String(), nil
}

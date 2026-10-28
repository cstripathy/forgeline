package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/forgelinehq/forgeline/internal/store"
	"github.com/forgelinehq/forgeline/internal/version"
)

const (
	idAWS         = "aws"
	cmdTimeout    = 25 * time.Second
	maxCmdOutput  = 4 << 20
	awsCacheTTL   = 20 * time.Second
	costCacheTTL  = time.Hour
	infraSection  = 20 * time.Second
	stepContainer = "forgeline-"
)

// stepContainerRE matches the names the Docker executor gives step containers: forgeline- and
// 12 hex digits. Other containers that merely start with "forgeline-" are not steps.
var stepContainerRE = regexp.MustCompile(`^forgeline-[0-9a-f]{12}$`)

// cmdRunner runs an external program. It exists so tests can stand in for docker and aws.
type cmdRunner func(ctx context.Context, name string, args ...string) (stdout []byte, stderr string, err error)

type extraEnvKey struct{}

// withExtraEnv attaches environment variables for one command run through a cmdRunner.
func withExtraEnv(ctx context.Context, env []string) context.Context {
	return context.WithValue(ctx, extraEnvKey{}, env)
}

func execRunner(ctx context.Context, name string, args ...string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(cmd.Environ(), "AWS_PAGER=", "AWS_RETRY_MODE=standard", "AWS_MAX_ATTEMPTS=2")
	if extra, ok := ctx.Value(extraEnvKey{}).([]string); ok {
		cmd.Env = mergeEnv(cmd.Env, extra)
	}
	var out, errb limitedBuffer
	out.max, errb.max = maxCmdOutput, 1<<16
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.b.Bytes(), strings.TrimSpace(errb.b.String()), err
}

// mergeEnv applies extra to base. A NAME= entry with no value removes NAME: the AWS CLI reads an
// empty AWS_PROFILE as a profile called "" and fails, so "cleared" has to mean "unset".
func mergeEnv(base, extra []string) []string {
	drop := map[string]bool{}
	for _, e := range extra {
		name, _, _ := strings.Cut(e, "=")
		drop[name] = true
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, e := range base {
		if name, _, _ := strings.Cut(e, "="); !drop[name] {
			out = append(out, e)
		}
	}
	for _, e := range extra {
		if _, v, _ := strings.Cut(e, "="); v != "" {
			out = append(out, e)
		}
	}
	return out
}

type limitedBuffer struct {
	b   bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil // never fail the process; extra output is dropped
}

// --- runtime overview ---

// RuntimeJSON describes the Forgeline server and the Docker host its steps run on.
type RuntimeJSON struct {
	Server       ServerInfoJSON      `json:"server"`
	Docker       DockerJSON          `json:"docker"`
	Steps        []StepContainerJSON `json:"steps"`
	Security     SecurityJSON        `json:"security"`
	Integrations map[string]any      `json:"integrations"`
}

// ServerInfoJSON is the Forgeline process itself.
type ServerInfoJSON struct {
	Version        string    `json:"version"`
	StartedAt      time.Time `json:"started_at"`
	UptimeSeconds  int64     `json:"uptime_seconds"`
	Go             string    `json:"go"`
	Platform       string    `json:"platform"`
	DataDir        string    `json:"data_dir"`
	DiskTotalBytes uint64    `json:"disk_total_bytes"`
	DiskFreeBytes  uint64    `json:"disk_free_bytes"`
	Store          string    `json:"store"`
	MaxRuns        int       `json:"max_runs"`
	Parallelism    int       `json:"parallelism"`
	Running        int       `json:"running"`
	Queued         int       `json:"queued"`
}

// DockerJSON is the container runtime steps use.
type DockerJSON struct {
	Available         bool   `json:"available"`
	Error             string `json:"error,omitempty"`
	Version           string `json:"version,omitempty"`
	Host              string `json:"host,omitempty"`
	OS                string `json:"os,omitempty"`
	CPUs              int    `json:"cpus,omitempty"`
	MemoryBytes       int64  `json:"memory_bytes,omitempty"`
	StorageDriver     string `json:"storage_driver,omitempty"`
	Containers        int    `json:"containers"`
	ContainersRunning int    `json:"containers_running"`
	Images            int    `json:"images"`
}

// StepContainerJSON is a step container running now.
type StepContainerJSON struct {
	Name    string `json:"name"`
	Image   string `json:"image"`
	Status  string `json:"status"`
	Running string `json:"running_for"`
}

// SecurityJSON summarises the trust configuration without revealing any of it.
type SecurityJSON struct {
	Users          int      `json:"users"`
	Admins         int      `json:"admins"`
	VaultSecrets   int      `json:"vault_secrets"`
	EnvSecrets     int      `json:"env_secrets"`
	DefaultNetwork string   `json:"default_network"`
	Notes          []string `json:"notes"`
}

func (s *Server) handleRuntime(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := RuntimeJSON{Steps: []StepContainerJSON{}, Integrations: map[string]any{}}
	out.Server = ServerInfoJSON{
		Version: version.Version, StartedAt: s.started, UptimeSeconds: int64(time.Since(s.started).Seconds()),
		Go: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH, DataDir: s.cfg.DataDir,
		Store: s.cfg.StoreKind, MaxRuns: s.cfg.MaxRuns, Parallelism: s.cfg.Parallelism,
	}
	if total, free, ok := diskUsage(s.cfg.DataDir); ok {
		out.Server.DiskTotalBytes, out.Server.DiskFreeBytes = total, free
	}
	if runs, err := s.cfg.Store.ListRuns(ctx, store.ListOptions{Limit: overviewWindow}); err == nil {
		for _, run := range runs {
			switch run.Status {
			case store.RunRunning:
				out.Server.Running++
			case store.RunQueued:
				out.Server.Queued++
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); out.Docker = s.dockerInfo(ctx) }()
	go func() { defer wg.Done(); out.Steps = s.stepContainers(ctx) }()

	users, _ := s.loadUsers(ctx)
	out.Security.Users = len(users)
	for _, u := range users {
		if u.Role == "admin" && !u.Disabled {
			out.Security.Admins++
		}
	}
	if names, err := s.secretNames(ctx); err == nil {
		out.Security.EnvSecrets = len(s.cfg.Secrets)
		out.Security.VaultSecrets = len(names) - len(s.cfg.Secrets)
	}
	out.Security.DefaultNetwork = "egress"
	out.Security.Notes = []string{
		"Steps run in containers with no-new-privileges. Whether a step has network access is set per step (permissions.network).",
		"Runs started by GitHub receive no secrets until trust levels exist (design note 005, slice 3).",
	}
	if s.cfg.KeySource == "file" || s.cfg.KeySource == "ephemeral" {
		out.Security.Notes = append(out.Security.Notes, map[string]string{
			"file":      "The vault key is stored next to the data it protects. Set FORGELINE_MASTER_KEY from a secret manager for a real deployment.",
			"ephemeral": "The vault key exists only in memory: stored secrets are lost when the server restarts.",
		}[s.cfg.KeySource])
	}
	gh := s.githubView(ctx, r)
	out.Integrations["github"] = map[string]any{"active": gh.Active, "source": gh.Source, "repositories": len(gh.Rules)}
	var aws awsSettings
	_ = s.docs.Get(ctx, kindSettings, idAWS, &aws)
	out.Integrations["aws"] = map[string]any{"enabled": aws.Enabled, "profile": aws.Profile, "region": aws.Region}
	wg.Wait()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) dockerInfo(ctx context.Context) DockerJSON {
	out, errText, err := s.run(ctx, "docker", "info", "--format", "{{json .}}")
	if err != nil {
		msg := errText
		if msg == "" {
			msg = err.Error()
		}
		return DockerJSON{Error: truncate(msg, 300)}
	}
	var raw struct {
		ServerVersion     string
		Name              string
		OperatingSystem   string
		NCPU              int
		MemTotal          int64
		Driver            string
		Containers        int
		ContainersRunning int
		Images            int
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return DockerJSON{Error: "could not read docker info: " + err.Error()}
	}
	return DockerJSON{Available: true, Version: raw.ServerVersion, Host: raw.Name, OS: raw.OperatingSystem, CPUs: raw.NCPU,
		MemoryBytes: raw.MemTotal, StorageDriver: raw.Driver, Containers: raw.Containers,
		ContainersRunning: raw.ContainersRunning, Images: raw.Images}
}

func (s *Server) stepContainers(ctx context.Context) []StepContainerJSON {
	out, _, err := s.run(ctx, "docker", "ps", "--filter", "name="+stepContainer, "--format", "{{json .}}")
	list := []StepContainerJSON{}
	if err != nil {
		return list
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var c struct{ Names, Image, Status, RunningFor string }
		if json.Unmarshal([]byte(line), &c) != nil || !stepContainerRE.MatchString(c.Names) {
			continue
		}
		list = append(list, StepContainerJSON{Name: c.Names, Image: c.Image, Status: c.Status, Running: c.RunningFor})
	}
	return list
}

// --- AWS ---

type awsSettings struct {
	Enabled   bool      `json:"enabled"`
	Profile   string    `json:"profile,omitempty"`
	Region    string    `json:"region,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

var (
	awsProfileRE = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)
	awsRegionRE  = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
)

// awsArgs prefixes args with the profile and region from the console settings.
func (s *Server) awsArgs(st awsSettings, args ...string) []string {
	var pre []string
	if st.Profile != "" && st.Profile != ssoProfile { // the console's SSO sign-in uses credentials in the environment, not a profile
		pre = append(pre, "--profile", st.Profile)
	}
	if st.Region != "" {
		pre = append(pre, "--region", st.Region)
	}
	return append(append(pre, args...), "--output", "json")
}

func (s *Server) aws(ctx context.Context, st awsSettings, args ...string) ([]byte, error) {
	if st.Profile == ssoProfile {
		env, err := s.ssoEnv(ctx)
		if err != nil {
			return nil, err
		}
		ctx = withExtraEnv(ctx, env)
	}
	out, errText, err := s.run(ctx, "aws", s.awsArgs(st, args...)...)
	if err != nil {
		return nil, awsError(errText, err, st.Profile)
	}
	return out, nil
}

// awsError turns a failed aws call into something a person can act on.
func awsError(stderr string, err error, profile string) error {
	var nf *exec.Error
	if errors.As(err, &nf) {
		return errors.New("the AWS CLI is not installed on the server")
	}
	low := strings.ToLower(stderr)
	switch {
	case strings.Contains(low, "sso") && (strings.Contains(low, "expired") || strings.Contains(low, "token") || strings.Contains(low, "login")):
		cmd := "aws sso login"
		if profile != "" {
			cmd += " --profile " + profile
		}
		return fmt.Errorf("the AWS session expired; run `%s` on the server and try again", cmd)
	case strings.Contains(low, "unable to locate credentials"), strings.Contains(low, "could not be found"):
		return errors.New("no AWS credentials: the profile does not exist or is not logged in on the server")
	case strings.Contains(low, "accessdenied"), strings.Contains(low, "unauthorizedoperation"), strings.Contains(low, "not authorized"):
		return fmt.Errorf("AWS denied the request: %s", truncate(firstLine(stderr), 240))
	}
	if stderr == "" {
		return err
	}
	return errors.New(truncate(firstLine(stderr), 300))
}

func (s *Server) loadAWS(ctx context.Context) awsSettings {
	var st awsSettings
	_ = s.docs.Get(ctx, kindSettings, idAWS, &st)
	return st
}

// AWSIdentityJSON is who the server is in AWS.
type AWSIdentityJSON struct {
	Account string `json:"account"`
	ARN     string `json:"arn"`
	UserID  string `json:"user_id"`
}

// AWSStackJSON is a CloudFormation stack.
type AWSStackJSON struct {
	Name        string            `json:"name"`
	Status      string            `json:"status"`
	Reason      string            `json:"reason,omitempty"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at,omitempty"`
	Description string            `json:"description,omitempty"`
	Outputs     map[string]string `json:"outputs"`
}

// AWSInstanceJSON is an EC2 instance.
type AWSInstanceJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	State     string `json:"state"`
	PublicIP  string `json:"public_ip,omitempty"`
	PrivateIP string `json:"private_ip,omitempty"`
	LaunchAt  string `json:"launch_time,omitempty"`
	Arch      string `json:"architecture,omitempty"`
	Stack     string `json:"stack,omitempty"`
}

// AWSJSON is the read-only view of the connected AWS account.
type AWSJSON struct {
	Enabled   bool   `json:"enabled"`
	Profile   string `json:"profile,omitempty"`
	Region    string `json:"region,omitempty"`
	FetchedAt string `json:"fetched_at,omitempty"`
	// Error is set when the account cannot be reached at all, e.g. an expired login.
	Error     string            `json:"error,omitempty"`
	Identity  *AWSIdentityJSON  `json:"identity,omitempty"`
	Stacks    []AWSStackJSON    `json:"stacks"`
	Instances []AWSInstanceJSON `json:"instances"`
	// SectionErrors names parts that failed while others worked (usually permissions).
	SectionErrors map[string]string `json:"section_errors,omitempty"`
}

type awsCache struct {
	mu   sync.Mutex
	at   time.Time
	key  string
	data AWSJSON
	cost *costCache
}

type costCache struct {
	at   time.Time
	key  string
	data CostJSON
}

func (s *Server) handleAWS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := s.loadAWS(ctx)
	if !st.Enabled {
		writeJSON(w, http.StatusOK, AWSJSON{Enabled: false, Stacks: []AWSStackJSON{}, Instances: []AWSInstanceJSON{}})
		return
	}
	key := st.Profile + "|" + st.Region
	s.awsCache.mu.Lock()
	if r.URL.Query().Get("refresh") != "1" && s.awsCache.key == key && time.Since(s.awsCache.at) < awsCacheTTL {
		d := s.awsCache.data
		s.awsCache.mu.Unlock()
		writeJSON(w, http.StatusOK, d)
		return
	}
	s.awsCache.mu.Unlock()

	d := s.fetchAWS(ctx, st)
	if d.Error == "" {
		s.awsCache.mu.Lock()
		s.awsCache.key, s.awsCache.at, s.awsCache.data = key, time.Now(), d
		s.awsCache.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) fetchAWS(ctx context.Context, st awsSettings) AWSJSON {
	d := AWSJSON{Enabled: true, Profile: st.Profile, Region: st.Region, FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Stacks: []AWSStackJSON{}, Instances: []AWSInstanceJSON{}, SectionErrors: map[string]string{}}
	ctx, cancel := context.WithTimeout(ctx, infraSection)
	defer cancel()

	// Identity first: if it fails nothing else will work, and the message is the useful one.
	idOut, err := s.aws(ctx, st, "sts", "get-caller-identity")
	if err != nil {
		d.Error = err.Error()
		return d
	}
	var id struct{ Account, Arn, UserId string }
	json.Unmarshal(idOut, &id)
	d.Identity = &AWSIdentityJSON{Account: id.Account, ARN: id.Arn, UserID: id.UserId}

	var wg sync.WaitGroup
	var mu sync.Mutex
	section := func(name string, f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil {
				mu.Lock()
				d.SectionErrors[name] = err.Error()
				mu.Unlock()
			}
		}()
	}
	section("stacks", func() error {
		out, err := s.aws(ctx, st, "cloudformation", "describe-stacks")
		if err != nil {
			return err
		}
		var raw struct {
			Stacks []struct {
				StackName, StackStatus, StackStatusReason, Description string
				CreationTime, LastUpdatedTime                          string
				Outputs                                                []struct{ OutputKey, OutputValue string }
			}
		}
		if err := json.Unmarshal(out, &raw); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		for _, x := range raw.Stacks {
			sj := AWSStackJSON{Name: x.StackName, Status: x.StackStatus, Reason: x.StackStatusReason, CreatedAt: x.CreationTime,
				UpdatedAt: x.LastUpdatedTime, Description: x.Description, Outputs: map[string]string{}}
			for _, o := range x.Outputs {
				sj.Outputs[o.OutputKey] = o.OutputValue
			}
			d.Stacks = append(d.Stacks, sj)
		}
		sort.Slice(d.Stacks, func(i, j int) bool { return d.Stacks[i].Name < d.Stacks[j].Name })
		return nil
	})
	section("instances", func() error {
		out, err := s.aws(ctx, st, "ec2", "describe-instances")
		if err != nil {
			return err
		}
		var raw struct {
			Reservations []struct {
				Instances []struct {
					InstanceId, InstanceType, PublicIpAddress, PrivateIpAddress, Architecture, LaunchTime string
					State                                                                                 struct{ Name string }
					Tags                                                                                  []struct{ Key, Value string }
				}
			}
		}
		if err := json.Unmarshal(out, &raw); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		for _, res := range raw.Reservations {
			for _, in := range res.Instances {
				ij := AWSInstanceJSON{ID: in.InstanceId, Type: in.InstanceType, State: in.State.Name, PublicIP: in.PublicIpAddress,
					PrivateIP: in.PrivateIpAddress, LaunchAt: in.LaunchTime, Arch: in.Architecture}
				for _, t := range in.Tags {
					switch t.Key {
					case "Name":
						ij.Name = t.Value
					case "aws:cloudformation:stack-name":
						ij.Stack = t.Value
					}
				}
				d.Instances = append(d.Instances, ij)
			}
		}
		sort.Slice(d.Instances, func(i, j int) bool { return d.Instances[i].ID < d.Instances[j].ID })
		return nil
	})
	wg.Wait()
	if len(d.SectionErrors) == 0 {
		d.SectionErrors = nil
	}
	return d
}

// CostJSON is spend over the last 30 days, by service.
type CostJSON struct {
	Available bool          `json:"available"`
	Error     string        `json:"error,omitempty"`
	From      string        `json:"from,omitempty"`
	To        string        `json:"to,omitempty"`
	Total     float64       `json:"total"`
	Unit      string        `json:"unit,omitempty"`
	Services  []CostService `json:"services"`
	FetchedAt string        `json:"fetched_at,omitempty"`
}

// CostService is one line of the cost breakdown.
type CostService struct {
	Service string  `json:"service"`
	Amount  float64 `json:"amount"`
}

// handleAWSCost reads Cost Explorer. Each call is billed by AWS, so the result is kept for an hour
// and the console asks for it only on request.
func (s *Server) handleAWSCost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := s.loadAWS(ctx)
	if !st.Enabled {
		writeError(w, http.StatusConflict, "connect an AWS account first")
		return
	}
	key := st.Profile
	s.awsCache.mu.Lock()
	if c := s.awsCache.cost; c != nil && c.key == key && time.Since(c.at) < costCacheTTL && r.URL.Query().Get("refresh") != "1" {
		d := c.data
		s.awsCache.mu.Unlock()
		writeJSON(w, http.StatusOK, d)
		return
	}
	s.awsCache.mu.Unlock()

	now := time.Now().UTC()
	from, to := now.AddDate(0, 0, -30).Format("2006-01-02"), now.AddDate(0, 0, 1).Format("2006-01-02")
	// Cost Explorer is a global service homed in us-east-1.
	ce := st
	ce.Region = "us-east-1"
	out, err := s.aws(ctx, ce, "ce", "get-cost-and-usage", "--time-period", "Start="+from+",End="+to,
		"--granularity", "MONTHLY", "--metrics", "UnblendedCost", "--group-by", "Type=DIMENSION,Key=SERVICE")
	d := CostJSON{Services: []CostService{}, From: from, To: to, FetchedAt: now.Format(time.RFC3339)}
	if err != nil {
		d.Error = err.Error()
		writeJSON(w, http.StatusOK, d)
		return
	}
	var raw struct {
		ResultsByTime []struct {
			Groups []struct {
				Keys    []string
				Metrics map[string]struct{ Amount, Unit string }
			}
		}
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		d.Error = "could not read the cost report: " + err.Error()
		writeJSON(w, http.StatusOK, d)
		return
	}
	byService := map[string]float64{}
	for _, period := range raw.ResultsByTime {
		for _, g := range period.Groups {
			if len(g.Keys) == 0 {
				continue
			}
			m := g.Metrics["UnblendedCost"]
			var amount float64
			fmt.Sscanf(m.Amount, "%f", &amount)
			byService[g.Keys[0]] += amount
			if d.Unit == "" {
				d.Unit = m.Unit
			}
		}
	}
	for name, amt := range byService {
		d.Total += amt
		if amt >= 0.005 {
			d.Services = append(d.Services, CostService{Service: name, Amount: amt})
		}
	}
	sort.Slice(d.Services, func(i, j int) bool { return d.Services[i].Amount > d.Services[j].Amount })
	d.Available = true
	s.awsCache.mu.Lock()
	s.awsCache.cost = &costCache{at: time.Now(), key: key, data: d}
	s.awsCache.mu.Unlock()
	writeJSON(w, http.StatusOK, d)
}

type putAWSRequest struct {
	Enabled bool   `json:"enabled"`
	Profile string `json:"profile"`
	Region  string `json:"region"`
}

// AWSSettingsResult is the saved connection and the outcome of testing it.
type AWSSettingsResult struct {
	Enabled  bool             `json:"enabled"`
	Profile  string           `json:"profile"`
	Region   string           `json:"region"`
	Identity *AWSIdentityJSON `json:"identity,omitempty"`
	Error    string           `json:"error,omitempty"`
}

func (s *Server) handleGetAWSSettings(w http.ResponseWriter, r *http.Request) {
	st := s.loadAWS(r.Context())
	writeJSON(w, http.StatusOK, AWSSettingsResult{Enabled: st.Enabled, Profile: st.Profile, Region: st.Region})
}

func (s *Server) handlePutAWSSettings(w http.ResponseWriter, r *http.Request) {
	var req putAWSRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Profile, req.Region = strings.TrimSpace(req.Profile), strings.TrimSpace(req.Region)
	if req.Profile != "" && !awsProfileRE.MatchString(req.Profile) {
		writeError(w, http.StatusBadRequest, "the profile name may contain only letters, digits and . _ @ -")
		return
	}
	if req.Region != "" && !awsRegionRE.MatchString(req.Region) {
		writeError(w, http.StatusBadRequest, "the region must look like ap-south-1")
		return
	}
	if req.Enabled && req.Region == "" {
		writeError(w, http.StatusBadRequest, "choose a region")
		return
	}
	if req.Profile == ssoProfile {
		writeError(w, http.StatusBadRequest, "that profile name is reserved for signing in from the console")
		return
	}
	p := principalFrom(r.Context())
	st := awsSettings{Enabled: req.Enabled, Profile: req.Profile, Region: req.Region, UpdatedAt: time.Now().UTC(), UpdatedBy: p.Name}
	s.applyAWSSettings(w, r, st)
}

// applyAWSSettings proves the connection works, saves it and answers. It returns false when it
// refused (and has already answered).
func (s *Server) applyAWSSettings(w http.ResponseWriter, r *http.Request, st awsSettings) bool {
	res := AWSSettingsResult{Enabled: st.Enabled, Profile: st.Profile, Region: st.Region}
	if st.Enabled {
		out, err := s.aws(r.Context(), st, "sts", "get-caller-identity")
		if err != nil {
			// Not saved: a connection that cannot work would only break the infrastructure page.
			res.Error = err.Error()
			s.auditReq(r, "integration.aws.update", "integration/aws", "profile "+st.Profile+" refused: "+err.Error(), false)
			writeJSON(w, http.StatusUnprocessableEntity, res)
			return false
		}
		var id struct{ Account, Arn, UserId string }
		json.Unmarshal(out, &id)
		res.Identity = &AWSIdentityJSON{Account: id.Account, ARN: id.Arn, UserID: id.UserId}
	}
	if err := s.docs.Put(r.Context(), kindSettings, idAWS, st); err != nil {
		s.internalError(w, "saving the settings", err)
		return false
	}
	s.awsCache.mu.Lock()
	s.awsCache.key, s.awsCache.cost = "", nil
	s.awsCache.mu.Unlock()
	detail := "disabled"
	if st.Enabled {
		detail = fmt.Sprintf("profile %q, region %s, account %s", st.Profile, st.Region, res.Identity.Account)
	}
	s.auditReq(r, "integration.aws.update", "integration/aws", detail, true)
	writeJSON(w, http.StatusOK, res)
	return true
}

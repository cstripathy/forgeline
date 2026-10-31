package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	awsdeploy "github.com/forgelinehq/forgeline/deploy/aws"
	"github.com/forgelinehq/forgeline/internal/authn"
	"github.com/forgelinehq/forgeline/internal/docstore"
)

// Environments (note 017, slice 6): temporary infrastructure and developer boxes. An environment is a
// CloudFormation stack made from a catalog template, with an owner, a purpose, a time to live and a
// cost estimate. It is requested by a developer, approved the same way any cloud change is, and
// deleted by Forgeline when its time is up. An administrator can set a self-service policy: requests
// for chosen templates, within a time and cost limit, start without waiting for a second person.
// That is the only way a stack gets created without one, and every such step is audited under the
// name of the policy.

const (
	kindEnv       = "environment"
	kindEnvPolicy = "envpolicy"
	maxEnvDocs    = 300
	envTick       = 5 * time.Second
	maxExtensions = 3
	maxExtendHrs  = 24

	envRequested    = "requested"    // waiting for approval, or for its preview
	envProvisioning = "provisioning" // approved; AWS is creating it
	envActive       = "active"
	envDestroying   = "destroying"
	envDestroyed    = "destroyed"
	envFailed       = "failed"
	envRejected     = "rejected"
)

// EnvPolicy is what developers may start on their own.
type EnvPolicy struct {
	SelfService      bool      `json:"self_service"`
	Templates        []string  `json:"templates"`
	MaxTTLHours      int       `json:"max_ttl_hours"`      // longest self-service request
	MaxLifetimeHours int       `json:"max_lifetime_hours"` // longest any environment may live, extensions included
	MaxActivePerUser int       `json:"max_active_per_user"`
	MaxMonthlyUSD    float64   `json:"max_monthly_usd"` // largest estimate that skips approval
	UpdatedBy        string    `json:"updated_by,omitempty"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
}

func (p EnvPolicy) withDefaults() EnvPolicy {
	if p.MaxTTLHours == 0 {
		p.MaxTTLHours = 24
	}
	if p.MaxLifetimeHours == 0 {
		p.MaxLifetimeHours = 168
	}
	if p.MaxActivePerUser == 0 {
		p.MaxActivePerUser = 2
	}
	if p.MaxMonthlyUSD == 0 {
		p.MaxMonthlyUSD = 40
	}
	if p.Templates == nil {
		p.Templates = []string{}
	}
	return p
}

func (s *Server) loadEnvPolicy(ctx context.Context) EnvPolicy {
	var p EnvPolicy
	_ = s.docs.Get(ctx, kindEnvPolicy, "policy", &p)
	return p.withDefaults()
}

// Environment is one temporary stack.
type Environment struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Template        string            `json:"template"`
	Purpose         string            `json:"purpose"`
	Owner           string            `json:"owner"`
	Stack           string            `json:"stack"`
	Params          map[string]string `json:"params,omitempty"`
	TTLHours        int               `json:"ttl_hours"`
	Status          string            `json:"status"`
	ChangeID        string            `json:"change_id"`
	DestroyChangeID string            `json:"destroy_change_id,omitempty"`
	AutoApprove     bool              `json:"auto_approve"`     // the request fits the self-service policy
	ViaAI           bool              `json:"via_ai,omitempty"` // the assistant, an agent or a workflow asked; a person always approves
	Approved        bool              `json:"approved,omitempty"`
	DestroyBy       string            `json:"destroy_by,omitempty"`
	Outcome         string            `json:"outcome,omitempty"` // failed, when a failed deploy is being cleaned up
	MonthlyUSD      float64           `json:"monthly_usd"`
	Extensions      int               `json:"extensions,omitempty"`
	Note            string            `json:"note,omitempty"`
	RequestedAt     time.Time         `json:"requested_at"`
	ActivatedAt     *time.Time        `json:"activated_at,omitempty"`
	ExpiresAt       *time.Time        `json:"expires_at,omitempty"`
	FinishedAt      *time.Time        `json:"finished_at,omitempty"`
}

func (e Environment) live() bool {
	return e.Status == envRequested || e.Status == envProvisioning || e.Status == envActive || e.Status == envDestroying
}

// envSpec says what a template is as an environment and what it costs.
type envSpec struct {
	Kind       string
	DefaultTTL int
	AutoNet    bool // needs a VPC and subnet, which are found when not given
	UserParams []string
	Monthly    func(params map[string]string) float64
}

var devBoxPrices = map[string]float64{"t4g.small": 14.0, "t4g.medium": 28.0} // USD a month on demand, about

var envSpecs = map[string]envSpec{
	"dev-box": {Kind: "Developer box", DefaultTTL: 8, AutoNet: true, UserParams: []string{"InstanceType", "DiskGiB"},
		Monthly: func(p map[string]string) float64 {
			disk, _ := strconv.ParseFloat(p["DiskGiB"], 64)
			return devBoxPrices[p["InstanceType"]] + 0.10*disk + 3.65 // the instance, the disk, and the public IPv4 address
		}},
	"qa-sandbox": {Kind: "QA sandbox", DefaultTTL: 24, Monthly: func(map[string]string) float64 { return 1.0 }},
	// The three below cost nothing idle; the estimate allows for light test traffic.
	"lambda-sandbox":   {Kind: "Lambda sandbox", DefaultTTL: 24, Monthly: func(map[string]string) float64 { return 0.5 }},
	"event-pipeline":   {Kind: "Event pipeline", DefaultTTL: 24, Monthly: func(map[string]string) float64 { return 1.0 }},
	"workflow-sandbox": {Kind: "Workflow sandbox", DefaultTTL: 24, Monthly: func(map[string]string) float64 { return 0.5 }},
}

var ownerExpiryParams = []CatalogParam{
	{Name: "Owner", Label: "Owner", Required: true, pattern: regexp.MustCompile(`^[A-Za-z0-9_.@ -]{1,64}$`)},
	{Name: "ExpiresAt", Label: "Expires at (UTC)", Required: true, pattern: regexp.MustCompile(`^[0-9TZ:-]{20}$`)},
}

func init() {
	catalog = append(catalog,
		CatalogTemplate{ID: "dev-box", Title: "Developer box",
			Description:  "One small private EC2 instance with an encrypted disk and no inbound ports. Reach it with Session Manager. Deleted when its time is up.",
			DefaultStack: "dev-box",
			Parameters: []CatalogParam{
				{Name: "VpcId", Label: "VPC", Required: true, pattern: regexp.MustCompile(`^vpc-[0-9a-f]{8,17}$`)},
				{Name: "SubnetId", Label: "Public subnet", Required: true, pattern: regexp.MustCompile(`^subnet-[0-9a-f]{8,17}$`)},
				{Name: "InstanceType", Label: "Instance type", Default: "t4g.small", Options: []string{"t4g.small", "t4g.medium"}},
				{Name: "DiskGiB", Label: "Disk size (GiB)", Default: "30", pattern: regexp.MustCompile(`^(2[0-9]|[3-9][0-9]|100)$`)},
				{Name: "Owner", Label: "Owner", Required: true, pattern: regexp.MustCompile(`^[A-Za-z0-9_.@ -]{1,64}$`)},
				{Name: "ExpiresAt", Label: "Expires at (UTC)", Required: true, pattern: regexp.MustCompile(`^[0-9TZ:-]{20}$`)},
			},
			body: awsdeploy.DevBoxTemplate, capabilities: []string{"CAPABILITY_IAM"}},
		CatalogTemplate{ID: "qa-sandbox", Title: "QA sandbox",
			Description:  "A DynamoDB table and an encrypted SQS queue for a test run. Almost free while idle. Deleted when its time is up.",
			DefaultStack: "qa-sandbox",
			Parameters: []CatalogParam{
				{Name: "Owner", Label: "Owner", Required: true, pattern: regexp.MustCompile(`^[A-Za-z0-9_.@ -]{1,64}$`)},
				{Name: "ExpiresAt", Label: "Expires at (UTC)", Required: true, pattern: regexp.MustCompile(`^[0-9TZ:-]{20}$`)},
			},
			body: awsdeploy.QASandboxTemplate},
		CatalogTemplate{ID: "lambda-sandbox", Title: "Lambda sandbox",
			Description:  "One small Lambda function that echoes what it is given, behind a private function URL that needs AWS credentials, with logs kept for three days. Free while idle. Deleted when its time is up.",
			DefaultStack: "lambda-sandbox", Parameters: ownerExpiryParams, body: awsdeploy.LambdaSandboxTemplate, capabilities: []string{"CAPABILITY_IAM"}},
		CatalogTemplate{ID: "event-pipeline", Title: "Event pipeline",
			Description:  "An encrypted SNS topic that fans out to an encrypted SQS queue, with a dead-letter queue for messages that fail five times. Almost free while idle. Deleted when its time is up.",
			DefaultStack: "event-pipeline", Parameters: ownerExpiryParams, body: awsdeploy.EventPipelineTemplate},
		CatalogTemplate{ID: "workflow-sandbox", Title: "Workflow sandbox",
			Description:  "A two-step Step Functions state machine whose role can call nothing in your account, for trying a workflow. Free while idle. Deleted when its time is up.",
			DefaultStack: "workflow-sandbox", Parameters: ownerExpiryParams, body: awsdeploy.WorkflowSandboxTemplate, capabilities: []string{"CAPABILITY_IAM"}})
}

type envEngine struct{ mu sync.Mutex }

// --- estimates and eligibility ---

type envQuote struct {
	MonthlyUSD  float64 `json:"monthly_usd"`
	ForTTLUSD   float64 `json:"for_ttl_usd"`
	SelfService bool    `json:"self_service"`
	Why         string  `json:"why"` // why it needs a person, when it does
}

// quote prices a request and says whether the policy lets it start on its own.
func quote(pol EnvPolicy, template string, params map[string]string, ttl int) envQuote {
	spec := envSpecs[template]
	monthly := spec.Monthly(params)
	q := envQuote{MonthlyUSD: round2(monthly), ForTTLUSD: round2(monthly * float64(ttl) / 730)}
	switch {
	case !pol.SelfService:
		q.Why = "Self-service is off, so an administrator approves each environment."
	case !contains(pol.Templates, template):
		q.Why = "This template is not on the self-service list."
	case ttl > pol.MaxTTLHours:
		q.Why = fmt.Sprintf("Self-service environments live at most %d hours.", pol.MaxTTLHours)
	case monthly > pol.MaxMonthlyUSD:
		q.Why = fmt.Sprintf("It costs about $%.0f a month, over the self-service limit of $%.0f.", monthly, pol.MaxMonthlyUSD)
	default:
		q.SelfService = true
	}
	return q
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// defaultNetwork finds the default VPC and one of its public subnets.
func (s *Server) defaultNetwork(ctx context.Context, st awsSettings) (string, string, error) {
	out, err := s.aws(ctx, st, "ec2", "describe-vpcs", "--filters", "Name=isDefault,Values=true")
	if err != nil {
		return "", "", err
	}
	var v struct{ Vpcs []struct{ VpcId string } }
	if json.Unmarshal(out, &v) != nil || len(v.Vpcs) == 0 {
		return "", "", errors.New("this account has no default VPC, so a developer box needs a VPC and subnet to be given")
	}
	out, err = s.aws(ctx, st, "ec2", "describe-subnets", "--filters", "Name=vpc-id,Values="+v.Vpcs[0].VpcId, "Name=map-public-ip-on-launch,Values=true")
	if err != nil {
		return "", "", err
	}
	var sn struct{ Subnets []struct{ SubnetId string } }
	if json.Unmarshal(out, &sn) != nil || len(sn.Subnets) == 0 {
		return "", "", errors.New("the default VPC has no public subnet")
	}
	ids := make([]string, 0, len(sn.Subnets))
	for _, x := range sn.Subnets {
		ids = append(ids, x.SubnetId)
	}
	sort.Strings(ids)
	return v.Vpcs[0].VpcId, ids[0], nil
}

// --- handlers ---

func (s *Server) listEnvs(ctx context.Context) ([]Environment, error) {
	docs, err := s.docs.List(ctx, kindEnv, docstore.ListOptions{Desc: true, Limit: maxEnvDocs})
	if err != nil {
		return nil, err
	}
	return docstore.Decode[Environment](docs)
}

func (s *Server) loadEnv(ctx context.Context, id string) (Environment, error) {
	var e Environment
	err := s.docs.Get(ctx, kindEnv, id, &e)
	return e, err
}

func (s *Server) handleListEnvs(w http.ResponseWriter, r *http.Request) {
	es, err := s.listEnvs(r.Context())
	if err != nil {
		s.internalError(w, "reading the environments", err)
		return
	}
	if es == nil {
		es = []Environment{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"environments": es})
}

func (s *Server) handleGetEnv(w http.ResponseWriter, r *http.Request) {
	e, err := s.loadEnv(r.Context(), r.PathValue("id"))
	if err != nil {
		s.defError(w, "environment", err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleEnvTemplates(w http.ResponseWriter, r *http.Request) {
	var out []map[string]any
	for _, t := range catalog {
		spec, ok := envSpecs[t.ID]
		if !ok {
			continue
		}
		ps := []CatalogParam{} // a template with nothing to choose still sends a list, not null
		for _, p := range t.Parameters {
			if contains(spec.UserParams, p.Name) {
				ps = append(ps, p)
			}
		}
		ph := map[string]string{}
		for _, p := range t.Parameters {
			switch p.Name {
			case "VpcId":
				ph[p.Name] = "vpc-00000000"
			case "SubnetId":
				ph[p.Name] = "subnet-00000000"
			case "Owner":
				ph[p.Name] = "x"
			case "ExpiresAt":
				ph[p.Name] = "2026-01-01T00:00:00Z"
			}
		}
		defaults, _ := checkParams(t, ph)
		out = append(out, map[string]any{"id": t.ID, "title": t.Title, "description": t.Description, "kind": spec.Kind, "default_ttl_hours": spec.DefaultTTL,
			"params": ps, "monthly_usd": round2(spec.Monthly(defaults))})
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

type envRequest struct {
	Template string            `json:"template"`
	Name     string            `json:"name"`
	Purpose  string            `json:"purpose"`
	TTLHours int               `json:"ttl_hours"`
	Params   map[string]string `json:"params"`
}

// prepareEnv checks a request and builds the parameters the template needs.
func (s *Server) prepareEnv(ctx context.Context, p Principal, req envRequest, pol EnvPolicy) (CatalogTemplate, map[string]string, *httpErr) {
	spec, ok := envSpecs[req.Template]
	tpl, found := findTemplate(req.Template)
	if !ok || !found {
		return tpl, nil, refusal(http.StatusBadRequest, "choose one of the environment templates")
	}
	user := map[string]string{}
	for k, v := range req.Params {
		if !contains(spec.UserParams, k) {
			return tpl, nil, refusal(http.StatusBadRequest, fmt.Sprintf("%q is not something you can set for this template", k))
		}
		user[k] = v
	}
	if req.TTLHours < 1 || req.TTLHours > pol.MaxLifetimeHours {
		return tpl, nil, refusal(http.StatusBadRequest, fmt.Sprintf("an environment lives between 1 and %d hours", pol.MaxLifetimeHours))
	}
	exp := time.Now().UTC().Add(time.Duration(req.TTLHours) * time.Hour).Format("2006-01-02T15:04:05Z")
	user["Owner"], user["ExpiresAt"] = p.Name, exp
	if spec.AutoNet {
		st := s.loadAWS(ctx)
		if !st.Enabled {
			return tpl, nil, refusal(http.StatusConflict, "connect an AWS account first (Infrastructure → AWS)")
		}
		vpc, subnet, err := s.defaultNetwork(ctx, st)
		if err != nil {
			return tpl, nil, refusal(http.StatusConflict, err.Error())
		}
		user["VpcId"], user["SubnetId"] = vpc, subnet
	}
	params, err := checkParams(tpl, user)
	if err != nil {
		return tpl, nil, refusal(http.StatusBadRequest, err.Error())
	}
	return tpl, params, nil
}

func (s *Server) handleEnvEstimate(w http.ResponseWriter, r *http.Request) {
	var req envRequest
	if !readJSON(w, r, &req) {
		return
	}
	spec, ok := envSpecs[req.Template]
	tpl, found := findTemplate(req.Template)
	if !ok || !found {
		writeError(w, http.StatusBadRequest, "choose one of the environment templates")
		return
	}
	pol := s.loadEnvPolicy(r.Context())
	if req.TTLHours < 1 || req.TTLHours > pol.MaxLifetimeHours {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("an environment lives between 1 and %d hours", pol.MaxLifetimeHours))
		return
	}
	// Placeholders for what the server decides, only where the template has them.
	user := map[string]string{}
	for _, p := range tpl.Parameters {
		switch p.Name {
		case "VpcId":
			user[p.Name] = "vpc-00000000"
		case "SubnetId":
			user[p.Name] = "subnet-00000000"
		case "Owner":
			user[p.Name] = "x"
		case "ExpiresAt":
			user[p.Name] = "2026-01-01T00:00:00Z"
		}
	}
	for k, v := range req.Params {
		if contains(spec.UserParams, k) {
			user[k] = v
		}
	}
	params, err := checkParams(tpl, user)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, quote(pol, req.Template, params, req.TTLHours))
}

var envNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 -]{2,39}$`)

func (s *Server) handleRequestEnv(w http.ResponseWriter, r *http.Request) {
	var req envRequest
	if !readJSON(w, r, &req) {
		return
	}
	e, herr := s.requestEnv(r.Context(), principalFrom(r.Context()), clientIP(r), req, false)
	if herr != nil {
		if herr.status >= 500 {
			s.internalError(w, "requesting the environment", errors.New(herr.msg))
			return
		}
		writeError(w, herr.status, herr.msg)
		return
	}
	writeJSON(w, http.StatusAccepted, e)
}

// requestEnv records an environment and the cloud change that creates it. viaAI is true when the
// assistant, an agent or a workflow asked on a person's behalf: such a request never starts on its
// own, whatever the self-service policy says, because nobody has looked at it yet.
func (s *Server) requestEnv(ctx context.Context, p Principal, ip string, req envRequest, viaAI bool) (Environment, *httpErr) {
	req.Name, req.Purpose = strings.TrimSpace(req.Name), strings.TrimSpace(req.Purpose)
	switch {
	case !envNameRE.MatchString(req.Name):
		return Environment{}, refusal(http.StatusBadRequest, "name it with 3 to 40 letters, digits, spaces or hyphens, starting with a letter")
	case len(req.Purpose) < 5 || len(req.Purpose) > 300:
		return Environment{}, refusal(http.StatusBadRequest, "say what it is for in 5 to 300 characters; the approver reads it")
	}
	pol := s.loadEnvPolicy(ctx)
	// One environment at a time per person up to the limit, whoever approves.
	all, err := s.listEnvs(ctx)
	if err != nil {
		return Environment{}, refusal(http.StatusInternalServerError, "reading the environments: "+err.Error())
	}
	active := 0
	for _, e := range all {
		if e.Owner == p.Name && e.live() {
			active++
		}
	}
	if active >= pol.MaxActivePerUser {
		return Environment{}, refusal(http.StatusConflict, fmt.Sprintf("you already have %d environments; delete one before asking for another (the limit is %d)", active, pol.MaxActivePerUser))
	}
	tpl, params, herr := s.prepareEnv(ctx, p, req, pol)
	if herr != nil {
		return Environment{}, herr
	}
	q := quote(pol, req.Template, params, req.TTLHours)
	if viaAI {
		q.SelfService = false
	}
	id := randomID()[:12]
	stack := "env-" + strings.Trim(slugRE.ReplaceAllString(strings.ToLower(req.Name), "-"), "-")
	if len(stack) > 40 {
		stack = strings.TrimRight(stack[:40], "-")
	}
	stack += "-" + id[:4]
	reason := fmt.Sprintf("Environment %q for %s: %s (%s, %d hours, about $%.2f)", req.Name, p.Name, req.Purpose, tpl.Title, req.TTLHours, q.ForTTLUSD)
	if viaAI {
		reason += ". Asked for by the assistant; no person has reviewed the request yet"
	}
	c, herr := s.proposeChange(ctx, p, ip, proposeRequest{Kind: kindStackDeploy, Reason: truncate(reason, 500), Template: req.Template, Stack: stack, Params: params})
	if herr != nil {
		return Environment{}, herr
	}
	e := Environment{ID: id, Name: req.Name, Template: req.Template, Purpose: req.Purpose, Owner: p.Name, Stack: stack, Params: params, TTLHours: req.TTLHours,
		Status: envRequested, ChangeID: c.ID, AutoApprove: q.SelfService, ViaAI: viaAI, MonthlyUSD: q.MonthlyUSD, RequestedAt: time.Now().UTC()}
	if err := s.docs.Put(ctx, kindEnv, e.ID, e); err != nil {
		return Environment{}, refusal(http.StatusInternalServerError, "saving the environment: "+err.Error())
	}
	s.pruneEnvs(ctx)
	how := "waits for an administrator"
	if q.SelfService {
		how = "within the self-service policy"
	} else if viaAI {
		how = "asked for by the assistant, waits for an administrator"
	}
	_ = s.audit(ctx, p, ip, "env.request", "env/"+e.ID, fmt.Sprintf("%s (%s) %dh $%.2f/month, %s", e.Name, e.Template, e.TTLHours, e.MonthlyUSD, how), true)
	return e, nil
}

func (s *Server) pruneEnvs(ctx context.Context) {
	all, err := s.listEnvs(ctx)
	if err != nil || len(all) <= maxEnvDocs-20 {
		return
	}
	for _, e := range all[maxEnvDocs-20:] {
		if !e.live() {
			_ = s.docs.Delete(ctx, kindEnv, e.ID)
		}
	}
}

// ownerOrAdmin reports whether the caller may act on an environment.
func ownerOrAdmin(p Principal, e Environment) bool {
	return e.Owner == p.Name || p.Role.AtLeast(authn.Admin)
}

func (s *Server) handleExtendEnv(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hours int `json:"hours"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	p := principalFrom(ctx)
	s.envs.mu.Lock()
	defer s.envs.mu.Unlock()
	e, err := s.loadEnv(ctx, r.PathValue("id"))
	if err != nil {
		s.defError(w, "environment", err)
		return
	}
	pol := s.loadEnvPolicy(ctx)
	switch {
	case !ownerOrAdmin(p, e):
		writeError(w, http.StatusForbidden, "only its owner or an administrator can extend an environment")
		return
	case e.Status != envActive || e.ExpiresAt == nil || e.ActivatedAt == nil:
		writeError(w, http.StatusConflict, "only an active environment can be extended")
		return
	case req.Hours < 1 || req.Hours > maxExtendHrs:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("extend it by 1 to %d hours at a time", maxExtendHrs))
		return
	case e.Extensions >= maxExtensions && !p.Role.AtLeast(authn.Admin):
		writeError(w, http.StatusConflict, fmt.Sprintf("an environment can be extended %d times; ask an administrator or request a new one", maxExtensions))
		return
	}
	next := e.ExpiresAt.Add(time.Duration(req.Hours) * time.Hour)
	if limit := e.ActivatedAt.Add(time.Duration(pol.MaxLifetimeHours) * time.Hour); next.After(limit) {
		writeError(w, http.StatusConflict, fmt.Sprintf("that is past the longest an environment may live (%d hours from when it started)", pol.MaxLifetimeHours))
		return
	}
	e.ExpiresAt, e.Extensions = &next, e.Extensions+1
	if err := s.docs.Put(ctx, kindEnv, e.ID, e); err != nil {
		s.internalError(w, "saving the environment", err)
		return
	}
	s.auditReq(r, "env.extend", "env/"+e.ID, fmt.Sprintf("by %d hours, now until %s", req.Hours, next.Format(time.RFC3339)), true)
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleDestroyEnv(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	s.envs.mu.Lock()
	defer s.envs.mu.Unlock()
	e, err := s.loadEnv(ctx, r.PathValue("id"))
	if err != nil {
		s.defError(w, "environment", err)
		return
	}
	switch {
	case !ownerOrAdmin(p, e):
		writeError(w, http.StatusForbidden, "only its owner or an administrator can delete an environment")
		return
	case e.Status != envActive && e.Status != envFailed:
		writeError(w, http.StatusConflict, "this environment is "+e.Status+", so there is nothing to delete yet")
		return
	}
	if herr := s.startDestroy(ctx, &e, "owner:"+p.Name, "deleted by "+p.Name, clientIP(r)); herr != nil {
		writeError(w, herr.status, herr.msg)
		return
	}
	s.auditReq(r, "env.destroy", "env/"+e.ID, e.Name, true)
	writeJSON(w, http.StatusAccepted, e)
}

func (s *Server) handleEnvPolicy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.loadEnvPolicy(r.Context()))
}

func (s *Server) handlePutEnvPolicy(w http.ResponseWriter, r *http.Request) {
	var req EnvPolicy
	if !readJSON(w, r, &req) {
		return
	}
	switch {
	case req.MaxTTLHours < 1 || req.MaxTTLHours > 168:
		writeError(w, http.StatusBadRequest, "self-service environments live 1 to 168 hours")
		return
	case req.MaxLifetimeHours < req.MaxTTLHours || req.MaxLifetimeHours > 720:
		writeError(w, http.StatusBadRequest, "the longest life is at least the self-service time and at most 720 hours")
		return
	case req.MaxActivePerUser < 1 || req.MaxActivePerUser > 10:
		writeError(w, http.StatusBadRequest, "each person may have 1 to 10 environments")
		return
	case req.MaxMonthlyUSD < 1 || req.MaxMonthlyUSD > 500:
		writeError(w, http.StatusBadRequest, "the self-service cost limit is $1 to $500 a month")
		return
	}
	for _, t := range req.Templates {
		if _, ok := envSpecs[t]; !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%q is not an environment template", t))
			return
		}
	}
	req.UpdatedBy, req.UpdatedAt = principalFrom(r.Context()).Name, time.Now().UTC()
	if err := s.docs.Put(r.Context(), kindEnvPolicy, "policy", req); err != nil {
		s.internalError(w, "saving the policy", err)
		return
	}
	s.auditReq(r, "env.policy", "env/policy", fmt.Sprintf("self-service %v, templates %v, up to %dh and $%.0f/month", req.SelfService, req.Templates, req.MaxTTLHours, req.MaxMonthlyUSD), true)
	writeJSON(w, http.StatusOK, req)
}

// --- the engine ---

// startDestroy asks for the stack to be deleted and records who is behind it. The request goes through
// the same change machinery; the engine approves it under the named policy when its preview is ready.
func (s *Server) startDestroy(ctx context.Context, e *Environment, by, why, ip string) *httpErr {
	c, herr := s.proposeChange(ctx, Principal{Name: by, Role: authn.Admin}, ip, proposeRequest{Kind: kindStackDelete, Stack: e.Stack,
		Reason: fmt.Sprintf("Delete environment %q (%s)", e.Name, why)})
	if herr != nil {
		return herr
	}
	keepNote := e.Status == envFailed // a failed creation keeps its own reason; the cleanup is not the news
	if keepNote {
		e.Outcome = envFailed
	}
	e.Status, e.DestroyChangeID, e.DestroyBy = envDestroying, c.ID, by
	if !keepNote {
		e.Note = why
	}
	return refusalIf(s.docs.Put(ctx, kindEnv, e.ID, *e))
}

func refusalIf(err error) *httpErr {
	if err != nil {
		return refusal(http.StatusInternalServerError, err.Error())
	}
	return nil
}

func (s *Server) startEnvs(ctx context.Context) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(envTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				s.envTick(ctx, now)
			}
		}
	}()
}

// envTick moves every living environment forward one step. It is safe to call often.
func (s *Server) envTick(ctx context.Context, now time.Time) {
	es, err := s.listEnvs(ctx)
	if err != nil {
		return
	}
	for _, e := range es {
		if e.live() {
			s.advanceEnv(ctx, e, now)
		}
	}
}

func (s *Server) advanceEnv(ctx context.Context, e Environment, now time.Time) {
	s.envs.mu.Lock()
	defer s.envs.mu.Unlock()
	cur, err := s.loadEnv(ctx, e.ID) // another step may have moved it
	if err != nil || !cur.live() {
		return
	}
	e = cur
	save := func() { _ = s.docs.Put(ctx, kindEnv, e.ID, e) }
	finish := func(status, note string) {
		t := now
		e.Status, e.FinishedAt = status, &t
		if note != "" {
			e.Note = note
		}
		save()
	}
	switch e.Status {
	case envRequested, envProvisioning:
		c, err := s.loadChange(ctx, e.ChangeID)
		if err != nil {
			finish(envFailed, "the change for this environment is missing")
			return
		}
		switch c.Status {
		case stProposed:
			if e.AutoApprove && !e.Approved {
				_, herr := s.approveChange(ctx, c.ID, Principal{Name: "policy:self-service", Role: authn.Admin}, "", "within the self-service policy", true)
				switch {
				case herr == nil:
					e.Approved = true
					save()
					_ = s.audit(ctx, Principal{Name: "policy:self-service", Role: authn.Admin}, "", "env.approve", "env/"+e.ID, e.Name+" for "+e.Owner, true)
				case herr.status == http.StatusConflict && strings.Contains(herr.msg, "already running"):
					// try again next tick
				default:
					finish(envRejected, "the self-service approval was refused: "+herr.msg)
				}
			}
		case stExecuting:
			if e.Status != envProvisioning {
				e.Status = envProvisioning
				save()
			}
		case stSucceeded:
			exp := now.Add(time.Duration(e.TTLHours) * time.Hour)
			t := now
			e.Status, e.ActivatedAt, e.ExpiresAt = envActive, &t, &exp
			save()
			_ = s.audit(ctx, Principal{Name: "policy:environments", Role: authn.Admin}, "", "env.activate", "env/"+e.ID, fmt.Sprintf("%s for %s until %s", e.Name, e.Owner, exp.Format(time.RFC3339)), true)
		case stFailed:
			// A failed create can leave a stack in ROLLBACK_COMPLETE, which costs nothing but blocks the name.
			e.Status, e.Note = envFailed, "it could not be created: "+truncate(c.Result, 300)
			if herr := s.startDestroy(ctx, &e, "policy:cleanup", "cleaning up a failed creation", ""); herr != nil {
				finish(envFailed, e.Note)
			}
		case stRejected, stCanceled, stExpired:
			finish(envRejected, "the request was "+c.Status+" and nothing was created")
		}
	case envActive:
		if e.ExpiresAt != nil && !now.Before(*e.ExpiresAt) {
			if herr := s.startDestroy(ctx, &e, "policy:expiry", "its time is up", ""); herr != nil {
				e.Note = "could not start deleting it yet: " + herr.msg
				save()
				return
			}
			_ = s.audit(ctx, Principal{Name: "policy:expiry", Role: authn.Admin}, "", "env.expire", "env/"+e.ID, e.Name+" for "+e.Owner, true)
		}
	case envDestroying:
		c, err := s.loadChange(ctx, e.DestroyChangeID)
		if err != nil {
			finish(envFailed, "the delete request is missing")
			return
		}
		switch c.Status {
		case stProposed:
			if _, herr := s.approveChange(ctx, c.ID, Principal{Name: e.DestroyBy, Role: authn.Admin}, "", "environment delete requested by "+e.DestroyBy, true); herr != nil && !(herr.status == http.StatusConflict && strings.Contains(herr.msg, "already running")) {
				finish(envFailed, "could not delete it: "+herr.msg+". An administrator must remove stack "+e.Stack+".")
			}
		case stSucceeded:
			if e.Outcome == envFailed {
				finish(envFailed, e.Note)
			} else {
				finish(envDestroyed, "")
			}
		case stFailed, stRejected, stCanceled, stExpired:
			msg := "could not delete it: " + truncate(c.Result, 200)
			if e.Outcome == envFailed && strings.Contains(strings.ToLower(c.Result), "does not exist") {
				finish(envFailed, e.Note) // the create failed before a stack existed, so there is nothing left
				return
			}
			finish(envFailed, msg+" An administrator must remove stack "+e.Stack+".")
		}
	}
}

// failInterruptedEnvs needs nothing: every step is recorded, and the next tick carries on from it.

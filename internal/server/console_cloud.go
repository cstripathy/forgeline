package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	awsdeploy "github.com/forgelinehq/forgeline/deploy/aws"
	"github.com/forgelinehq/forgeline/internal/authn"
	"github.com/forgelinehq/forgeline/internal/docstore"
)

const (
	kindCloud       = "cloudchange"
	changeTTL       = 24 * time.Hour
	execTimeout     = 30 * time.Minute
	maxCloudChanges = 100
)

// Cloud change kinds and statuses.
const (
	kindStackDeploy    = "stack-deploy"
	kindStackDelete    = "stack-delete"
	kindInstanceAction = "instance-action"

	stPreparing = "preparing"
	stProposed  = "proposed"
	stExecuting = "executing"
	stSucceeded = "succeeded"
	stFailed    = "failed"
	stRejected  = "rejected"
	stCanceled  = "canceled"
	stExpired   = "expired"
)

// cloudState holds what the workflow needs beyond the stored changes.
type cloudState struct {
	mu   sync.Mutex
	busy map[string]string // target -> change id, for changes executing now
	poll atomic.Int64      // nanoseconds between polls of AWS while waiting; tests shorten it
}

func (c *cloudState) interval() time.Duration {
	if d := time.Duration(c.poll.Load()); d > 0 {
		return d
	}
	return 5 * time.Second
}

func (c *cloudState) setInterval(d time.Duration) { c.poll.Store(int64(d)) }

// ChangePreview is one line of what a change would do.
type ChangePreview struct {
	Action      string `json:"action"`
	Resource    string `json:"resource"`
	Type        string `json:"type"`
	Replacement string `json:"replacement,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// CloudChange is a proposed, approved or finished change to the AWS account.
type CloudChange struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Status string `json:"status"`
	// Reason is the proposer's explanation, required, and shown to the approver.
	Reason     string            `json:"reason"`
	Stack      string            `json:"stack,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
	InstanceID string            `json:"instance_id,omitempty"`
	Action     string            `json:"action,omitempty"`

	ChangeSet     string          `json:"change_set,omitempty"`
	ChangeSetType string          `json:"change_set_type,omitempty"`
	Preview       []ChangePreview `json:"preview"`
	PreviewNote   string          `json:"preview_note,omitempty"`

	Profile string `json:"profile,omitempty"`
	Region  string `json:"region"`
	Account string `json:"account"`

	ProposedBy   string     `json:"proposed_by"`
	ProposedAt   time.Time  `json:"proposed_at"`
	DecidedBy    string     `json:"decided_by,omitempty"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	DecisionNote string     `json:"decision_note,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Result       string     `json:"result,omitempty"`
}

func (c CloudChange) target() string {
	switch c.Kind {
	case kindInstanceAction:
		return "instance/" + c.InstanceID
	default:
		return "stack/" + c.Stack
	}
}

// --- catalog ---

// CatalogParam describes one input of a deployable template.
type CatalogParam struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Help     string   `json:"help,omitempty"`
	Required bool     `json:"required"`
	Default  string   `json:"default,omitempty"`
	Options  []string `json:"options,omitempty"`
	pattern  *regexp.Regexp
}

// CatalogTemplate is something the console can deploy.
type CatalogTemplate struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Description  string         `json:"description"`
	DefaultStack string         `json:"default_stack"`
	Parameters   []CatalogParam `json:"parameters"`
	body         string
	capabilities []string
}

var demoTemplate = CatalogTemplate{
	ID: "forgeline-demo", Title: "Forgeline demo server",
	Description:  "One encrypted t4g instance running Forgeline and Postgres, with no inbound ports (reach it through SSM). About $14 a month while running.",
	DefaultStack: "forgeline-demo",
	Parameters: []CatalogParam{
		{Name: "GitRef", Label: "Commit to build", Help: "Full 40-character commit SHA of the Forgeline repository.", Required: true, pattern: regexp.MustCompile(`^[0-9a-f]{40}$`)},
		{Name: "VpcId", Label: "VPC", Required: true, pattern: regexp.MustCompile(`^vpc-[0-9a-f]{8,17}$`)},
		{Name: "SubnetId", Label: "Public subnet", Help: "A subnet in that VPC that assigns public IPv4 addresses.", Required: true, pattern: regexp.MustCompile(`^subnet-[0-9a-f]{8,17}$`)},
		{Name: "InstanceType", Label: "Instance type", Default: "t4g.small", Options: []string{"t4g.small", "t4g.medium"}},
		{Name: "GoVersion", Label: "Go version", Default: "1.27.3", pattern: regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)},
		{Name: "TokenParameterName", Label: "SSM parameter holding the token", Help: "Create it first as a SecureString; the template never stores the token.", Default: "/forgeline-demo/token", pattern: regexp.MustCompile(`^/[A-Za-z0-9_./-]{1,256}$`)},
	},
	body:         awsdeploy.DemoTemplate,
	capabilities: []string{"CAPABILITY_IAM"},
}

var catalog = []CatalogTemplate{demoTemplate}

var (
	stackNameRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,127}$`)
	instanceIDRE = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)
)

// protectedStack names stacks the console will never delete, whatever role asks.
func protectedStack(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "cdktoolkit")
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"templates": catalog, "instance_actions": []string{"start", "stop", "reboot"}})
}

// --- listing ---

func (s *Server) loadChanges(ctx context.Context, limit int) ([]CloudChange, error) {
	docs, err := s.docs.List(ctx, kindCloud, docstore.ListOptions{Desc: true, Limit: limit})
	if err != nil {
		return nil, err
	}
	out, err := docstore.Decode[CloudChange](docs)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i] = expireIfStale(out[i])
	}
	return out, nil
}

// expireIfStale shows a proposal nobody acted on as expired. The stored record is updated when
// someone next touches it.
func expireIfStale(c CloudChange) CloudChange {
	if c.Status == stProposed && time.Since(c.ProposedAt) > changeTTL {
		c.Status = stExpired
	}
	return c
}

func (s *Server) handleListChanges(w http.ResponseWriter, r *http.Request) {
	changes, err := s.loadChanges(r.Context(), maxCloudChanges)
	if err != nil {
		s.internalError(w, "listing changes", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": changes})
}

func (s *Server) handleGetChange(w http.ResponseWriter, r *http.Request) {
	var c CloudChange
	if err := s.docs.Get(r.Context(), kindCloud, r.PathValue("id"), &c); errors.Is(err, docstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such change")
		return
	} else if err != nil {
		s.internalError(w, "reading the change", err)
		return
	}
	writeJSON(w, http.StatusOK, expireIfStale(c))
}

// --- proposing ---

type proposeRequest struct {
	Kind       string            `json:"kind"`
	Reason     string            `json:"reason"`
	Template   string            `json:"template"`
	Stack      string            `json:"stack"`
	Params     map[string]string `json:"params"`
	InstanceID string            `json:"instance_id"`
	Action     string            `json:"action"`
}

// httpErr is a refusal with the status the HTTP handler should answer.
type httpErr struct {
	status int
	msg    string
}

func (e *httpErr) Error() string { return e.msg }

func refusal(status int, msg string) *httpErr { return &httpErr{status, msg} }

func (s *Server) handlePropose(w http.ResponseWriter, r *http.Request) {
	var req proposeRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	c, herr := s.proposeChange(ctx, principalFrom(ctx), clientIP(r), req)
	if herr != nil {
		if herr.status >= 500 {
			s.internalError(w, "saving the proposal", errors.New(herr.msg))
			return
		}
		writeError(w, herr.status, herr.msg)
		return
	}
	writeJSON(w, http.StatusAccepted, c)
}

// proposeChange records a change and prepares its preview in the background. The environment engine
// calls it too, as a person or as a named policy; nothing about approval is decided here.
func (s *Server) proposeChange(ctx context.Context, p Principal, ip string, req proposeRequest) (CloudChange, *httpErr) {
	st := s.loadAWS(ctx)
	if !st.Enabled {
		return CloudChange{}, refusal(http.StatusConflict, "connect an AWS account first (Infrastructure → AWS)")
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if len(req.Reason) < 5 || len(req.Reason) > 500 {
		return CloudChange{}, refusal(http.StatusBadRequest, "say why in 5 to 500 characters; the approver reads it")
	}
	c := CloudChange{ID: authn.NewID(), Kind: req.Kind, Status: stPreparing, Reason: req.Reason, Preview: []ChangePreview{},
		Profile: st.Profile, Region: st.Region, ProposedBy: p.Name, ProposedAt: time.Now().UTC()}

	var prepare func(context.Context, *CloudChange, awsSettings) error
	switch req.Kind {
	case kindStackDeploy:
		tpl, ok := findTemplate(req.Template)
		if !ok {
			return CloudChange{}, refusal(http.StatusBadRequest, "unknown template")
		}
		stack := req.Stack
		if stack == "" {
			stack = tpl.DefaultStack
		}
		if !stackNameRE.MatchString(stack) {
			return CloudChange{}, refusal(http.StatusBadRequest, "the stack name is letters, digits and hyphens, starting with a letter")
		}
		params, err := checkParams(tpl, req.Params)
		if err != nil {
			return CloudChange{}, refusal(http.StatusBadRequest, err.Error())
		}
		c.Stack, c.Params, c.Title = stack, params, fmt.Sprintf("Deploy %s as stack %s", tpl.Title, stack)
		prepare = func(ctx context.Context, c *CloudChange, st awsSettings) error {
			return s.prepareDeploy(ctx, c, st, tpl)
		}
	case kindStackDelete:
		if !stackNameRE.MatchString(req.Stack) {
			return CloudChange{}, refusal(http.StatusBadRequest, "give the stack name")
		}
		if protectedStack(req.Stack) {
			_ = s.audit(ctx, p, ip, "cloud.propose", "stack/"+req.Stack, "refused: protected stack", false)
			return CloudChange{}, refusal(http.StatusForbidden, "this stack holds Forgeline-unrelated bootstrap resources and cannot be deleted from the console")
		}
		c.Stack, c.Title = req.Stack, "Delete stack "+req.Stack
		prepare = s.prepareDelete
	case kindInstanceAction:
		if !instanceIDRE.MatchString(req.InstanceID) {
			return CloudChange{}, refusal(http.StatusBadRequest, "give an instance id such as i-0123456789abcdef0")
		}
		if req.Action != "start" && req.Action != "stop" && req.Action != "reboot" {
			return CloudChange{}, refusal(http.StatusBadRequest, "action must be start, stop or reboot")
		}
		c.InstanceID, c.Action, c.Title = req.InstanceID, req.Action, fmt.Sprintf("%s instance %s", strings.ToUpper(req.Action[:1])+req.Action[1:], req.InstanceID)
		prepare = s.prepareInstance
	default:
		return CloudChange{}, refusal(http.StatusBadRequest, "kind must be stack-deploy, stack-delete or instance-action")
	}

	// Bind the change to the account it was prepared against.
	idOut, err := s.aws(ctx, st, "sts", "get-caller-identity")
	if err != nil {
		return CloudChange{}, refusal(http.StatusBadGateway, err.Error())
	}
	var id struct{ Account string }
	json.Unmarshal(idOut, &id)
	c.Account = id.Account

	if err := s.docs.Create(ctx, kindCloud, c.ID, c); err != nil {
		return CloudChange{}, refusal(http.StatusInternalServerError, err.Error())
	}
	_ = s.audit(ctx, p, ip, "cloud.propose", c.target(), c.Title+" — "+c.Reason, true)
	// The worker owns its own copy; the response is what was saved, not what it will become.
	work := c
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		if err := prepare(pctx, &work, st); err != nil {
			work.Status, work.Result = stFailed, err.Error()
			now := time.Now().UTC()
			work.FinishedAt = &now
		} else {
			work.Status = stProposed
		}
		if err := s.docs.Put(context.WithoutCancel(ctx), kindCloud, work.ID, work); err != nil {
			s.log.Error("saving a prepared proposal", "id", work.ID, "err", err)
		}
	}()
	return c, nil
}

func findTemplate(id string) (CatalogTemplate, bool) {
	for _, t := range catalog {
		if t.ID == id {
			return t, true
		}
	}
	return CatalogTemplate{}, false
}

// checkParams validates inputs against the template, rejects unknown ones and fills defaults.
func checkParams(t CatalogTemplate, in map[string]string) (map[string]string, error) {
	known := map[string]CatalogParam{}
	for _, p := range t.Parameters {
		known[p.Name] = p
	}
	for k := range in {
		if _, ok := known[k]; !ok {
			return nil, fmt.Errorf("unknown parameter %q", k)
		}
	}
	out := map[string]string{}
	for _, p := range t.Parameters {
		v := strings.TrimSpace(in[p.Name])
		if v == "" {
			v = p.Default
		}
		if v == "" {
			if p.Required {
				return nil, fmt.Errorf("%s is required", p.Label)
			}
			continue
		}
		if len(p.Options) > 0 {
			ok := false
			for _, o := range p.Options {
				ok = ok || o == v
			}
			if !ok {
				return nil, fmt.Errorf("%s must be one of %s", p.Label, strings.Join(p.Options, ", "))
			}
		}
		if p.pattern != nil && !p.pattern.MatchString(v) {
			return nil, fmt.Errorf("%s is not valid: %q", p.Label, truncate(v, 60))
		}
		out[p.Name] = v
	}
	return out, nil
}

func (s *Server) stackStatus(ctx context.Context, st awsSettings, stack string) (status string, exists bool, err error) {
	out, e := s.aws(ctx, st, "cloudformation", "describe-stacks", "--stack-name", stack)
	if e != nil {
		if strings.Contains(strings.ToLower(e.Error()), "does not exist") {
			return "", false, nil
		}
		return "", false, e
	}
	var raw struct {
		Stacks []struct{ StackStatus string }
	}
	if err := json.Unmarshal(out, &raw); err != nil || len(raw.Stacks) == 0 {
		return "", false, fmt.Errorf("could not read stack %s", stack)
	}
	return raw.Stacks[0].StackStatus, true, nil
}

func (s *Server) prepareDeploy(ctx context.Context, c *CloudChange, st awsSettings, tpl CatalogTemplate) error {
	status, exists, err := s.stackStatus(ctx, st, c.Stack)
	if err != nil {
		return err
	}
	c.ChangeSetType = "CREATE"
	if exists {
		c.ChangeSetType = "UPDATE"
		if status == "REVIEW_IN_PROGRESS" {
			return fmt.Errorf("another proposal for stack %s is waiting for a decision; approve or reject it first", c.Stack)
		}
		if status == "ROLLBACK_COMPLETE" || status == "DELETE_FAILED" || strings.HasSuffix(status, "_IN_PROGRESS") {
			return fmt.Errorf("stack %s is %s, so it cannot be changed; delete it first or wait for it to settle", c.Stack, status)
		}
	}
	dir, err := os.MkdirTemp("", "forgeline-cfn-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	tplPath, paramPath := filepath.Join(dir, "template.yaml"), filepath.Join(dir, "params.json")
	if err := os.WriteFile(tplPath, []byte(tpl.body), 0o600); err != nil {
		return err
	}
	type kv struct{ ParameterKey, ParameterValue string }
	var kvs []kv
	for _, p := range tpl.Parameters {
		if v, ok := c.Params[p.Name]; ok {
			kvs = append(kvs, kv{p.Name, v})
		}
	}
	pj, _ := json.Marshal(kvs)
	if err := os.WriteFile(paramPath, pj, 0o600); err != nil {
		return err
	}
	c.ChangeSet = "forgeline-" + strings.ToLower(strings.NewReplacer("_", "x", "-", "y").Replace(c.ID))
	args := []string{"cloudformation", "create-change-set", "--stack-name", c.Stack, "--change-set-name", c.ChangeSet,
		"--change-set-type", c.ChangeSetType, "--template-body", "file://" + tplPath, "--parameters", "file://" + paramPath,
		"--tags", "Key=forgeline:managed,Value=true", "Key=forgeline:change,Value=" + c.ID}
	args = append(args, "--capabilities")
	args = append(args, tpl.capabilities...)
	if _, err := s.aws(ctx, st, args...); err != nil {
		return err
	}
	// Wait for AWS to compute the change set.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, err := s.aws(ctx, st, "cloudformation", "describe-change-set", "--stack-name", c.Stack, "--change-set-name", c.ChangeSet)
		if err != nil {
			return err
		}
		var cs struct {
			Status, StatusReason string
			Changes              []struct {
				ResourceChange struct {
					Action, LogicalResourceId, ResourceType, Replacement string
					Scope                                                []string
				}
			}
		}
		if err := json.Unmarshal(out, &cs); err != nil {
			return err
		}
		switch cs.Status {
		case "CREATE_COMPLETE":
			for _, ch := range cs.Changes {
				rc := ch.ResourceChange
				c.Preview = append(c.Preview, ChangePreview{Action: rc.Action, Resource: rc.LogicalResourceId, Type: rc.ResourceType,
					Replacement: rc.Replacement, Detail: strings.Join(rc.Scope, ", ")})
			}
			return nil
		case "FAILED":
			if strings.Contains(cs.StatusReason, "didn't contain changes") || strings.Contains(cs.StatusReason, "No updates") {
				s.cleanupChangeSet(ctx, st, c)
				return errors.New("nothing to change: the stack already matches this template and these parameters")
			}
			s.cleanupChangeSet(ctx, st, c)
			return fmt.Errorf("AWS could not build the change: %s", cs.StatusReason)
		}
		if time.Now().After(deadline) {
			s.cleanupChangeSet(ctx, st, c)
			return errors.New("AWS took too long to compute the change")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.cloud.interval() / 2):
		}
	}
}

// cleanupChangeSet removes an unused change set and, for a stack that only exists to hold it, the stack.
func (s *Server) cleanupChangeSet(ctx context.Context, st awsSettings, c *CloudChange) {
	ctx = context.WithoutCancel(ctx)
	if c.ChangeSet == "" {
		return
	}
	if _, err := s.aws(ctx, st, "cloudformation", "delete-change-set", "--stack-name", c.Stack, "--change-set-name", c.ChangeSet); err != nil {
		s.log.Warn("deleting a change set", "stack", c.Stack, "err", err)
	}
	if c.ChangeSetType == "CREATE" {
		if status, exists, _ := s.stackStatus(ctx, st, c.Stack); exists && status == "REVIEW_IN_PROGRESS" {
			if _, err := s.aws(ctx, st, "cloudformation", "delete-stack", "--stack-name", c.Stack); err != nil {
				s.log.Warn("deleting a review-only stack", "stack", c.Stack, "err", err)
			}
		}
	}
}

func (s *Server) prepareDelete(ctx context.Context, c *CloudChange, st awsSettings) error {
	_, exists, err := s.stackStatus(ctx, st, c.Stack)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("there is no stack named %s in this account and region", c.Stack)
	}
	out, err := s.aws(ctx, st, "cloudformation", "list-stack-resources", "--stack-name", c.Stack)
	if err != nil {
		return err
	}
	var raw struct {
		StackResourceSummaries []struct{ LogicalResourceId, ResourceType, PhysicalResourceId string }
	}
	json.Unmarshal(out, &raw)
	for _, r := range raw.StackResourceSummaries {
		c.Preview = append(c.Preview, ChangePreview{Action: "Remove", Resource: r.LogicalResourceId, Type: r.ResourceType, Detail: r.PhysicalResourceId})
	}
	if len(c.Preview) == 0 {
		c.PreviewNote = "AWS lists no resources for this stack."
	}
	return nil
}

func (s *Server) prepareInstance(ctx context.Context, c *CloudChange, st awsSettings) error {
	out, err := s.aws(ctx, st, "ec2", "describe-instances", "--instance-ids", c.InstanceID)
	if err != nil {
		return err
	}
	var raw struct {
		Reservations []struct {
			Instances []struct {
				InstanceType string
				State        struct{ Name string }
				Tags         []struct{ Key, Value string }
			}
		}
	}
	json.Unmarshal(out, &raw)
	if len(raw.Reservations) == 0 || len(raw.Reservations[0].Instances) == 0 {
		return fmt.Errorf("instance %s was not found", c.InstanceID)
	}
	in := raw.Reservations[0].Instances[0]
	name := c.InstanceID
	for _, t := range in.Tags {
		if t.Key == "Name" {
			name = t.Value + " (" + c.InstanceID + ")"
		}
	}
	state := in.State.Name
	switch {
	case c.Action == "start" && state != "stopped":
		return fmt.Errorf("%s is %s; only a stopped instance can be started", name, state)
	case c.Action == "stop" && state != "running":
		return fmt.Errorf("%s is %s; only a running instance can be stopped", name, state)
	case c.Action == "reboot" && state != "running":
		return fmt.Errorf("%s is %s; only a running instance can be rebooted", name, state)
	}
	c.Preview = append(c.Preview, ChangePreview{Action: strings.ToUpper(c.Action[:1]) + c.Action[1:], Resource: name, Type: "AWS::EC2::Instance",
		Detail: fmt.Sprintf("%s, now %s", in.InstanceType, state)})
	return nil
}

// --- deciding ---

type decideRequest struct {
	Note string `json:"note"`
}

func (s *Server) loadChange(ctx context.Context, id string) (CloudChange, error) {
	var c CloudChange
	err := s.docs.Get(ctx, kindCloud, id, &c)
	return expireIfStale(c), err
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var req decideRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	c, herr := s.approveChange(ctx, r.PathValue("id"), principalFrom(ctx), clientIP(r), req.Note, false)
	if herr != nil {
		if herr.status >= 500 {
			s.internalError(w, "approving the change", errors.New(herr.msg))
			return
		}
		writeError(w, herr.status, herr.msg)
		return
	}
	writeJSON(w, http.StatusAccepted, c)
}

// approveChange checks a proposed change is still safe to run and starts it. A person may not approve
// their own proposal. policy is true only for the environment engine acting under a rule an
// administrator set (self-service, expiry), which is the one case where the proposer is not a person.
func (s *Server) approveChange(ctx context.Context, id string, p Principal, ip, note string, policy bool) (CloudChange, *httpErr) {
	s.cloud.mu.Lock()
	c, err := s.loadChange(ctx, id)
	if errors.Is(err, docstore.ErrNotFound) {
		s.cloud.mu.Unlock()
		return CloudChange{}, refusal(http.StatusNotFound, "no such change")
	} else if err != nil {
		s.cloud.mu.Unlock()
		return CloudChange{}, refusal(http.StatusInternalServerError, err.Error())
	}
	refuse := func(status int, msg string) (CloudChange, *httpErr) {
		s.cloud.mu.Unlock()
		_ = s.audit(ctx, p, ip, "cloud.approve", c.target(), "refused: "+msg, false)
		return CloudChange{}, refusal(status, msg)
	}
	switch {
	case c.Status == stExpired:
		return refuse(http.StatusConflict, "this proposal is older than 24 hours; propose it again so the preview is current")
	case c.Status != stProposed:
		return refuse(http.StatusConflict, "this change is "+c.Status+", not waiting for approval")
	case c.ProposedBy == p.Name && !policy:
		return refuse(http.StatusForbidden, "you proposed this change; someone else must approve it")
	}
	if s.cloud.busy == nil {
		s.cloud.busy = map[string]string{}
	}
	if other, ok := s.cloud.busy[c.target()]; ok {
		return refuse(http.StatusConflict, "another change ("+other+") is already running against "+c.target())
	}
	st := s.loadAWS(ctx)
	if !st.Enabled || st.Profile != c.Profile || st.Region != c.Region {
		return refuse(http.StatusConflict, "the AWS connection changed since this was proposed; propose it again")
	}
	// The account must still be the one the preview was made against.
	idOut, err := s.aws(ctx, st, "sts", "get-caller-identity")
	if err != nil {
		s.cloud.mu.Unlock()
		return CloudChange{}, refusal(http.StatusBadGateway, err.Error())
	}
	var acct struct{ Account string }
	json.Unmarshal(idOut, &acct)
	if acct.Account != c.Account {
		return refuse(http.StatusConflict, fmt.Sprintf("the server is now in AWS account %s, but this was prepared for %s", acct.Account, c.Account))
	}

	now := time.Now().UTC()
	c.Status, c.DecidedBy, c.DecidedAt, c.DecisionNote = stExecuting, p.Name, &now, strings.TrimSpace(note)
	if err := s.docs.Put(ctx, kindCloud, c.ID, c); err != nil {
		s.cloud.mu.Unlock()
		return CloudChange{}, refusal(http.StatusInternalServerError, err.Error())
	}
	s.cloud.busy[c.target()] = c.ID
	s.cloud.mu.Unlock()
	_ = s.audit(ctx, p, ip, "cloud.approve", c.target(), c.Title+" — approved, executing", true)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.executeChange(context.WithoutCancel(ctx), c, st)
	}()
	return c, nil
}

func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	s.finishWithoutRunning(w, r, stRejected, true)
}

func (s *Server) handleCancelChange(w http.ResponseWriter, r *http.Request) {
	s.finishWithoutRunning(w, r, stCanceled, false)
}

// finishWithoutRunning rejects (admin) or cancels (proposer or admin) a change nobody has run.
func (s *Server) finishWithoutRunning(w http.ResponseWriter, r *http.Request, status string, needAdmin bool) {
	var req decideRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	p := principalFrom(ctx)
	s.cloud.mu.Lock()
	c, err := s.loadChange(ctx, r.PathValue("id"))
	if errors.Is(err, docstore.ErrNotFound) {
		s.cloud.mu.Unlock()
		writeError(w, http.StatusNotFound, "no such change")
		return
	} else if err != nil {
		s.cloud.mu.Unlock()
		s.internalError(w, "reading the change", err)
		return
	}
	if !p.Role.AtLeast(authn.Admin) && c.ProposedBy != p.Name {
		s.cloud.mu.Unlock()
		writeError(w, http.StatusForbidden, "only the proposer or an administrator can cancel a proposal")
		return
	}
	if c.Status != stProposed && c.Status != stExpired && c.Status != stPreparing {
		s.cloud.mu.Unlock()
		writeError(w, http.StatusConflict, "this change is "+c.Status+" and can no longer be "+status)
		return
	}
	now := time.Now().UTC()
	c.Status, c.DecidedBy, c.DecidedAt, c.FinishedAt, c.DecisionNote = status, p.Name, &now, &now, strings.TrimSpace(req.Note)
	if err := s.docs.Put(ctx, kindCloud, c.ID, c); err != nil {
		s.cloud.mu.Unlock()
		s.internalError(w, "saving the decision", err)
		return
	}
	s.cloud.mu.Unlock()
	if c.Kind == kindStackDeploy {
		st := awsSettings{Profile: c.Profile, Region: c.Region}
		s.cleanupChangeSet(ctx, st, &c)
	}
	s.auditReq(r, "cloud."+strings.TrimSuffix(status, "ed"), c.target(), c.Title, true)
	writeJSON(w, http.StatusOK, c)
}

// --- executing ---

func (s *Server) executeChange(ctx context.Context, c CloudChange, st awsSettings) {
	st = awsSettings{Profile: c.Profile, Region: c.Region}
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	var result string
	var err error
	switch c.Kind {
	case kindStackDeploy:
		result, err = s.executeDeploy(ctx, c, st)
	case kindStackDelete:
		result, err = s.executeDelete(ctx, c, st)
	case kindInstanceAction:
		result, err = s.executeInstance(ctx, c, st)
	}
	now := time.Now().UTC()
	c.FinishedAt, c.Result = &now, result
	c.Status = stSucceeded
	if err != nil {
		c.Status, c.Result = stFailed, err.Error()
	}
	bg := context.WithoutCancel(ctx)
	if perr := s.docs.Put(bg, kindCloud, c.ID, c); perr != nil {
		s.log.Error("saving a change result", "id", c.ID, "err", perr)
	}
	s.cloud.mu.Lock()
	delete(s.cloud.busy, c.target())
	s.cloud.mu.Unlock()
	s.audit(bg, Principal{Name: c.DecidedBy, Role: authn.Admin}, "", "cloud.execute", c.target(), c.Title+" — "+c.Status+": "+truncate(c.Result, 200), err == nil)
}

func (s *Server) waitStack(ctx context.Context, st awsSettings, stack string, done func(string) (bool, bool)) (string, error) {
	for {
		status, exists, err := s.stackStatus(ctx, st, stack)
		if err != nil {
			return "", err
		}
		if !exists {
			status = "DELETE_COMPLETE"
		}
		if finished, ok := done(status); finished {
			if !ok {
				return status, fmt.Errorf("the stack ended in %s; open the stack's events in the AWS console for the cause", status)
			}
			return status, nil
		}
		select {
		case <-ctx.Done():
			return status, fmt.Errorf("gave up waiting; the stack is %s", status)
		case <-time.After(s.cloud.interval()):
		}
	}
}

func (s *Server) executeDeploy(ctx context.Context, c CloudChange, st awsSettings) (string, error) {
	if _, err := s.aws(ctx, st, "cloudformation", "execute-change-set", "--stack-name", c.Stack, "--change-set-name", c.ChangeSet); err != nil {
		return "", err
	}
	status, err := s.waitStack(ctx, st, c.Stack, func(x string) (bool, bool) {
		switch x {
		case "CREATE_COMPLETE", "UPDATE_COMPLETE":
			return true, true
		}
		if strings.HasSuffix(x, "_FAILED") || strings.Contains(x, "ROLLBACK") || x == "DELETE_COMPLETE" {
			return true, false
		}
		return false, false
	})
	if err != nil {
		return "", err
	}
	return "Stack " + c.Stack + " is " + status, nil
}

func (s *Server) executeDelete(ctx context.Context, c CloudChange, st awsSettings) (string, error) {
	if _, err := s.aws(ctx, st, "cloudformation", "delete-stack", "--stack-name", c.Stack); err != nil {
		return "", err
	}
	if _, err := s.waitStack(ctx, st, c.Stack, func(x string) (bool, bool) {
		if x == "DELETE_COMPLETE" {
			return true, true
		}
		return x == "DELETE_FAILED", false
	}); err != nil {
		return "", err
	}
	return "Stack " + c.Stack + " was deleted", nil
}

func (s *Server) executeInstance(ctx context.Context, c CloudChange, st awsSettings) (string, error) {
	verb := map[string]string{"start": "start-instances", "stop": "stop-instances", "reboot": "reboot-instances"}[c.Action]
	if _, err := s.aws(ctx, st, "ec2", verb, "--instance-ids", c.InstanceID); err != nil {
		return "", err
	}
	want := map[string]string{"start": "running", "stop": "stopped"}[c.Action]
	if want == "" { // a reboot has no steady state to wait for
		return "Reboot requested for " + c.InstanceID, nil
	}
	for {
		out, err := s.aws(ctx, st, "ec2", "describe-instances", "--instance-ids", c.InstanceID)
		if err != nil {
			return "", err
		}
		var raw struct {
			Reservations []struct {
				Instances []struct{ State struct{ Name string } }
			}
		}
		json.Unmarshal(out, &raw)
		if len(raw.Reservations) > 0 && len(raw.Reservations[0].Instances) > 0 && raw.Reservations[0].Instances[0].State.Name == want {
			return fmt.Sprintf("%s is %s", c.InstanceID, want), nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%s did not reach %s in time", c.InstanceID, want)
		case <-time.After(s.cloud.interval()):
		}
	}
}

// failInterruptedChanges marks changes that were executing when the server stopped.
func (s *Server) failInterruptedChanges(ctx context.Context) {
	docs, err := s.docs.List(ctx, kindCloud, docstore.ListOptions{})
	if err != nil {
		return
	}
	changes, _ := docstore.Decode[CloudChange](docs)
	sort.Slice(changes, func(i, j int) bool { return changes[i].ID < changes[j].ID })
	for _, c := range changes {
		if c.Status != stExecuting && c.Status != stPreparing {
			continue
		}
		now := time.Now().UTC()
		c.FinishedAt = &now
		if c.Status == stExecuting {
			c.Status, c.Result = stFailed, "the server restarted while this was running; check the AWS page for the real state"
		} else {
			c.Status, c.Result = stFailed, "the server restarted before the preview was ready; propose it again"
		}
		s.docs.Put(ctx, kindCloud, c.ID, c)
	}
}

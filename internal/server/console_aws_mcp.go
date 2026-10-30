package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/forgelinehq/forgeline/internal/authn"
	"github.com/forgelinehq/forgeline/internal/docstore"
	"github.com/forgelinehq/forgeline/internal/mcp"
)

// The AWS MCP Server is AWS's managed MCP endpoint. Forgeline reaches it through AWS's signing
// proxy (mcp-proxy-for-aws, started as a child process with the console's AWS credentials) and
// uses it for two things: read-only lookups for the assistant, and applying a change that a person
// approved. The model never calls it directly: every script is read by checkScript first.
const (
	idAWSMCP          = "aws-mcp"
	kindAIProposal    = "aiproposal"
	defaultMCPProxy   = "uvx mcp-proxy-for-aws@1.6.0"
	defaultMCPAddress = "https://aws-mcp.us-east-1.api.aws/mcp"
	mcpStartTimeout   = 90 * time.Second
	mcpScriptTimeout  = 120 * time.Second
	aiProposalTTL     = 24 * time.Hour
	scriptToolName    = "aws___run_script"
)

// mcpConn is what the server needs from an MCP connection; tests replace the launcher.
type mcpConn interface {
	Tools(ctx context.Context) ([]mcp.Tool, error)
	Call(ctx context.Context, name string, args map[string]any) (mcp.Result, error)
	Close()
}

type awsMCPSettings struct {
	Enabled   bool      `json:"enabled"`
	Tools     []string  `json:"tools,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

func (s *Server) loadAWSMCP(ctx context.Context) awsMCPSettings {
	var st awsMCPSettings
	_ = s.docs.Get(ctx, kindSettings, idAWSMCP, &st)
	return st
}

// startAWSMCP launches the signing proxy for the console's AWS connection.
func (s *Server) startAWSMCP(ctx context.Context) (mcpConn, error) {
	st := s.loadAWS(ctx)
	if !st.Enabled {
		return nil, errors.New("connect an AWS account first (Infrastructure → AWS)")
	}
	if s.mcpStart != nil {
		return s.mcpStart(ctx, st)
	}
	proxy := strings.Fields(os.Getenv("FORGELINE_AWS_MCP_PROXY"))
	if len(proxy) == 0 {
		proxy = strings.Fields(defaultMCPProxy)
	}
	endpoint := os.Getenv("FORGELINE_AWS_MCP_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultMCPAddress
	}
	args := append(proxy[1:], endpoint, "--metadata", "AWS_REGION="+st.Region)
	// A clean environment: the proxy finds credentials through the profile or the SSO role the
	// console holds, and nothing else of the server's (token, master key) is passed on.
	env := []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	if st.Profile == ssoProfile {
		extra, err := s.ssoEnv(ctx)
		if err != nil {
			return nil, err
		}
		env = append(env, extra...)
	} else if st.Profile != "" {
		args = append(args, "--profile", st.Profile)
	}
	ctx, cancel := context.WithTimeout(ctx, mcpStartTimeout)
	defer cancel()
	cmd := exec.Command(proxy[0], args...)
	cmd.Env = env
	c, err := mcp.Start(ctx, cmd)
	if err != nil {
		var nf *exec.Error
		if errors.As(err, &nf) {
			return nil, fmt.Errorf("%s is not installed on the server (the AWS MCP proxy needs it)", proxy[0])
		}
		return nil, err
	}
	return c, nil
}

// callAWSMCP opens a connection, makes one call and closes it. A connection per call keeps the
// server free of long-lived child processes; the cost is a second or two of start-up.
func (s *Server) callAWSMCP(ctx context.Context, tool string, args map[string]any) (mcp.Result, error) {
	if !s.loadAWSMCP(ctx).Enabled {
		return mcp.Result{}, errors.New("the AWS MCP Server is not turned on (Infrastructure → AWS)")
	}
	c, err := s.startAWSMCP(ctx)
	if err != nil {
		return mcp.Result{}, err
	}
	defer c.Close()
	cctx, cancel := context.WithTimeout(ctx, mcpScriptTimeout)
	defer cancel()
	return c.Call(cctx, tool, args)
}

// --- settings ---

type AWSMCPJSON struct {
	Enabled  bool     `json:"enabled"`
	Tools    []string `json:"tools"`
	Proxy    string   `json:"proxy"`
	Endpoint string   `json:"endpoint"`
	Error    string   `json:"error,omitempty"`
}

func (s *Server) awsMCPView(st awsMCPSettings) AWSMCPJSON {
	proxy := os.Getenv("FORGELINE_AWS_MCP_PROXY")
	if proxy == "" {
		proxy = defaultMCPProxy
	}
	endpoint := os.Getenv("FORGELINE_AWS_MCP_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultMCPAddress
	}
	return AWSMCPJSON{Enabled: st.Enabled, Tools: st.Tools, Proxy: proxy, Endpoint: endpoint}
}

func (s *Server) handleGetAWSMCP(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.awsMCPView(s.loadAWSMCP(r.Context())))
}

func (s *Server) handlePutAWSMCP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	st := awsMCPSettings{Enabled: req.Enabled, UpdatedAt: time.Now().UTC(), UpdatedBy: principalFrom(r.Context()).Name}
	if req.Enabled {
		ctx, cancel := context.WithTimeout(r.Context(), mcpStartTimeout)
		defer cancel()
		c, err := s.startAWSMCP(ctx)
		if err == nil {
			defer c.Close()
			var tools []mcp.Tool
			if tools, err = c.Tools(ctx); err == nil {
				for _, t := range tools {
					st.Tools = append(st.Tools, t.Name)
				}
				sort.Strings(st.Tools)
				if !contains(st.Tools, scriptToolName) {
					err = fmt.Errorf("the server does not offer %s (it offers: %s)", scriptToolName, strings.Join(st.Tools, ", "))
				}
			}
		}
		if err != nil {
			v := s.awsMCPView(st)
			v.Enabled, v.Error = false, err.Error()
			s.auditReq(r, "integration.awsmcp.update", "integration/aws-mcp", "refused: "+err.Error(), false)
			writeJSON(w, http.StatusUnprocessableEntity, v)
			return
		}
	}
	if err := s.docs.Put(r.Context(), kindSettings, idAWSMCP, st); err != nil {
		s.internalError(w, "saving the settings", err)
		return
	}
	s.auditReq(r, "integration.awsmcp.update", "integration/aws-mcp", fmt.Sprintf("enabled %v, %d tools", st.Enabled, len(st.Tools)), true)
	writeJSON(w, http.StatusOK, s.awsMCPView(st))
}

func contains(l []string, x string) bool {
	for _, v := range l {
		if v == x {
			return true
		}
	}
	return false
}

// --- assistant lookups that use the MCP server ---

// scriptCalls reads the api_calls list the sandbox reports.
func scriptCalls(r mcp.Result) []scriptCall {
	var out struct {
		Calls []struct {
			Service   string `json:"service"`
			Operation string `json:"operation"`
			Status    string `json:"status"`
		} `json:"api_calls"`
	}
	raw := r.Structured
	if len(raw) == 0 {
		raw = json.RawMessage(r.Text)
	}
	_ = json.Unmarshal(raw, &out)
	var calls []scriptCall
	for _, c := range out.Calls {
		calls = append(calls, scriptCall{Service: c.Service, Operation: c.Operation})
	}
	return calls
}

// sameCalls reports whether the sandbox ran only calls the script declared (by service and
// operation; a call inside a loop may run many times).
func sameCalls(declared, ran []scriptCall) (extra string, ok bool) {
	want := map[string]bool{}
	for _, c := range declared {
		want[strings.ToLower(c.Service+" "+c.Operation)] = true
	}
	for _, c := range ran {
		if !want[strings.ToLower(c.Service+" "+c.Operation)] {
			return c.Service + " " + c.Operation, false
		}
	}
	return "", true
}

func (s *Server) lookupAWSScript(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Code string `json:"code"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	declared, err := checkScript(a.Code, false)
	if err != nil {
		return "", err
	}
	res, err := s.callAWSMCP(ctx, scriptToolName, map[string]any{"code": a.Code})
	if err != nil {
		return "", err
	}
	// Defence in depth: the script was read before it ran, and what the sandbox says it did must
	// match. If it does not, the result is thrown away and the event is reported.
	if extra, ok := sameCalls(declared, scriptCalls(res)); !ok {
		return "", fmt.Errorf("the sandbox ran %s, which the script did not declare; the result was discarded", extra)
	}
	if res.IsError {
		return "", errors.New("the script failed: " + truncate(res.Text, 600))
	}
	return truncate(res.Text, maxLookupResult), nil
}

// docs tools: forwarded with only the arguments each one takes.
var mcpDocTools = map[string]struct {
	tool string
	keys []string
}{
	"aws_search_documentation":      {"aws___search_documentation", []string{"search_phrase", "limit", "topics"}},
	"aws_read_documentation":        {"aws___read_documentation", []string{"requests"}},
	"aws_retrieve_skill":            {"aws___retrieve_skill", []string{"skill_name", "file"}},
	"aws_list_regions":              {"aws___list_regions", nil},
	"aws_get_regional_availability": {"aws___get_regional_availability", []string{"regions", "resource_type", "filters", "next_token", "region"}},
}

func (s *Server) lookupAWSDoc(name string) func(*Server, context.Context, json.RawMessage) (string, error) {
	return func(s *Server, ctx context.Context, raw json.RawMessage) (string, error) {
		var in map[string]any
		if err := decodeArgs(raw, &in); err != nil {
			return "", err
		}
		spec := mcpDocTools[name]
		args := map[string]any{}
		for _, k := range spec.keys {
			if v, ok := in[k]; ok {
				args[k] = v
			}
		}
		res, err := s.callAWSMCP(ctx, spec.tool, args)
		if err != nil {
			return "", err
		}
		if res.IsError {
			return "", errors.New(truncate(res.Text, 400))
		}
		return truncate(res.Text, maxLookupResult), nil
	}
}

// --- change proposals ---

// AIProposal is a change the assistant drafted. Nothing in it runs until a different administrator
// approves exactly this text.
type AIProposal struct {
	ID         string       `json:"id"`
	Title      string       `json:"title"`
	Reason     string       `json:"reason"`
	Code       string       `json:"code"`
	Calls      []scriptCall `json:"calls"`
	Hash       string       `json:"hash"`
	Risk       string       `json:"risk"`
	Target     string       `json:"target"` // profile and region it was drafted for
	Status     string       `json:"status"`
	ProposedBy string       `json:"proposed_by"`
	ProposedAt time.Time    `json:"proposed_at"`
	DecidedBy  string       `json:"decided_by,omitempty"`
	DecidedAt  *time.Time   `json:"decided_at,omitempty"`
	Note       string       `json:"note,omitempty"`
	Result     string       `json:"result,omitempty"`
	RanCalls   []scriptCall `json:"ran_calls,omitempty"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
	ViaAI      bool         `json:"via_ai"`
}

const (
	apProposed  = "proposed"
	apApplying  = "applying"
	apApplied   = "applied"
	apFailed    = "failed"
	apRejected  = "rejected"
	apCancelled = "cancelled"
	apExpired   = "expired"
)

func proposalHash(title, code, target string) string {
	sum := sha256.Sum256([]byte(title + "\x00" + code + "\x00" + target))
	return hex.EncodeToString(sum[:])
}

var riskyOp = regexp.MustCompile(`(Authorize|Attach|Associate|Modify.*Attribute|Put.*Policy|Enable|Update.*Security|Replace|Revoke)`)

func riskOf(calls []scriptCall) string {
	changes := 0
	for _, c := range calls {
		if !scriptReadOp.MatchString(c.Operation) {
			changes++
			if riskyOp.MatchString(c.Operation) {
				return "high"
			}
		}
	}
	if changes > 5 {
		return "high"
	}
	return "normal"
}

func expiredProposal(p AIProposal) AIProposal {
	if p.Status == apProposed && time.Since(p.ProposedAt) > aiProposalTTL {
		p.Status = apExpired
	}
	return p
}

func (s *Server) loadProposal(ctx context.Context, id string) (AIProposal, error) {
	var p AIProposal
	if err := s.docs.Get(ctx, kindAIProposal, id, &p); err != nil {
		return p, err
	}
	return expiredProposal(p), nil
}

// lookupProposeChange is the assistant's only way to ask for a change: it files a proposal.
func (s *Server) lookupProposeChange(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Title, Reason, Code string
	}
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	a.Title, a.Reason = strings.TrimSpace(a.Title), strings.TrimSpace(a.Reason)
	if len(a.Title) < 5 || len(a.Title) > 120 || len(a.Reason) < 10 || len(a.Reason) > 800 {
		return "", errors.New("give a title (5 to 120 characters) and a reason (10 to 800 characters); the approver reads both")
	}
	if !s.loadAWSMCP(ctx).Enabled {
		return "", errors.New("changes can only be proposed once the AWS MCP Server is turned on")
	}
	calls, err := checkScript(a.Code, true)
	if err != nil {
		return "", err
	}
	st := s.loadAWS(ctx)
	p := principalFrom(ctx)
	target := fmt.Sprintf("%s/%s", st.Profile, st.Region)
	prop := AIProposal{ID: authn.NewID(), Title: a.Title, Reason: a.Reason, Code: a.Code, Calls: calls,
		Hash: proposalHash(a.Title, a.Code, target), Risk: riskOf(calls), Target: target, Status: apProposed,
		ProposedBy: p.Name, ProposedAt: time.Now().UTC(), ViaAI: true}
	if err := s.docs.Put(ctx, kindAIProposal, prop.ID, prop); err != nil {
		return "", errors.New("could not save the proposal")
	}
	if sink, ok := ctx.Value(lookupSinkKey{}).(*lookupSink); ok {
		sink.proposals = append(sink.proposals, prop.ID)
	}
	s.audit(ctx, p, "", "ai.proposal.create", "ai-proposal/"+prop.ID, fmt.Sprintf("%s (%s risk, %d calls)", a.Title, prop.Risk, len(calls)), true)
	return fmt.Sprintf("Proposal %s filed (%s risk). Nothing has run. A different administrator must review and approve it in the console under Proposals; tell the person that.", prop.ID, prop.Risk), nil
}

type lookupSinkKey struct{}
type lookupSink struct{ proposals []string }

func (s *Server) handleListProposals(w http.ResponseWriter, r *http.Request) {
	docs, err := s.docs.List(r.Context(), kindAIProposal, docstore.ListOptions{Desc: true, Limit: 100})
	if err != nil {
		s.internalError(w, "reading the proposals", err)
		return
	}
	ps, err := docstore.Decode[AIProposal](docs)
	if err != nil {
		s.internalError(w, "reading the proposals", err)
		return
	}
	for i := range ps {
		ps[i] = expiredProposal(ps[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": ps})
}

func (s *Server) handleGetProposal(w http.ResponseWriter, r *http.Request) {
	p, err := s.loadProposal(r.Context(), r.PathValue("id"))
	if errors.Is(err, docstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such proposal")
		return
	} else if err != nil {
		s.internalError(w, "reading the proposal", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleApproveProposal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hash string `json:"hash"`
		Note string `json:"note"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	who := principalFrom(ctx)
	s.aiMu.Lock()
	p, err := s.loadProposal(ctx, r.PathValue("id"))
	if errors.Is(err, docstore.ErrNotFound) {
		s.aiMu.Unlock()
		writeError(w, http.StatusNotFound, "no such proposal")
		return
	} else if err != nil {
		s.aiMu.Unlock()
		s.internalError(w, "reading the proposal", err)
		return
	}
	refuse := func(status int, msg string) {
		s.aiMu.Unlock()
		s.auditReq(r, "ai.proposal.approve", "ai-proposal/"+p.ID, "refused: "+msg, false)
		writeError(w, status, msg)
	}
	st := s.loadAWS(ctx)
	switch {
	case p.Status == apExpired:
		refuse(http.StatusConflict, "this proposal is older than 24 hours; ask for it again so it matches the account today")
		return
	case p.Status != apProposed:
		refuse(http.StatusConflict, "this proposal is "+p.Status+", not waiting for approval")
		return
	case p.ProposedBy == who.Name:
		refuse(http.StatusForbidden, "you proposed this change; someone else must approve it")
		return
	case req.Hash != p.Hash || proposalHash(p.Title, p.Code, p.Target) != p.Hash:
		refuse(http.StatusConflict, "the text you approved is not the text of this proposal; reload and read it again")
		return
	case !st.Enabled || fmt.Sprintf("%s/%s", st.Profile, st.Region) != p.Target:
		refuse(http.StatusConflict, "the AWS connection changed since this was proposed; ask for it again")
		return
	case !s.loadAWSMCP(ctx).Enabled:
		refuse(http.StatusConflict, "the AWS MCP Server is turned off")
		return
	case s.aiBusy:
		refuse(http.StatusConflict, "another approved change is being applied; try again in a moment")
		return
	}
	// Re-read the script with the same rules, in case the rules are stricter than when it was filed.
	if _, err := checkScript(p.Code, true); err != nil {
		refuse(http.StatusUnprocessableEntity, "the script no longer passes the checks: "+err.Error())
		return
	}
	now := time.Now().UTC()
	p.Status, p.DecidedBy, p.DecidedAt, p.Note = apApplying, who.Name, &now, truncate(strings.TrimSpace(req.Note), 300)
	if err := s.docs.Put(ctx, kindAIProposal, p.ID, p); err != nil {
		s.aiMu.Unlock()
		s.internalError(w, "saving the approval", err)
		return
	}
	s.aiBusy = true
	s.aiMu.Unlock()
	s.auditReq(r, "ai.proposal.approve", "ai-proposal/"+p.ID, p.Title, true)
	go s.applyProposal(p, who)
	writeJSON(w, http.StatusAccepted, p)
}

// applyProposal runs the approved script, exactly as approved, and records what the sandbox says it did.
func (s *Server) applyProposal(p AIProposal, who Principal) {
	ctx, cancel := context.WithTimeout(context.Background(), mcpScriptTimeout+mcpStartTimeout)
	defer cancel()
	finish := func(status, result string, ran []scriptCall) {
		now := time.Now().UTC()
		p.Status, p.Result, p.RanCalls, p.FinishedAt = status, truncate(result, 6000), ran, &now
		_ = s.docs.Put(context.Background(), kindAIProposal, p.ID, p)
		s.aiMu.Lock()
		s.aiBusy = false
		s.aiMu.Unlock()
		s.audit(context.Background(), who, "", "ai.proposal.apply", "ai-proposal/"+p.ID, status+": "+truncate(result, 200), status == apApplied)
	}
	res, err := s.callAWSMCP(ctx, scriptToolName, map[string]any{"code": p.Code})
	if err != nil {
		finish(apFailed, err.Error(), nil)
		return
	}
	ran := scriptCalls(res)
	if extra, ok := sameCalls(p.Calls, ran); !ok {
		finish(apFailed, "the sandbox ran "+extra+", which was not in the approved script. Check the account by hand. "+res.Text, ran)
		return
	}
	if res.IsError || strings.Contains(res.Text, `"status": "error"`) {
		finish(apFailed, res.Text, ran)
		return
	}
	finish(apApplied, res.Text, ran)
}

func (s *Server) handleDecideProposal(reject bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		who := principalFrom(ctx)
		s.aiMu.Lock()
		defer s.aiMu.Unlock()
		p, err := s.loadProposal(ctx, r.PathValue("id"))
		if errors.Is(err, docstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such proposal")
			return
		} else if err != nil {
			s.internalError(w, "reading the proposal", err)
			return
		}
		if p.Status != apProposed && p.Status != apExpired {
			writeError(w, http.StatusConflict, "this proposal is "+p.Status)
			return
		}
		status, action := apCancelled, "ai.proposal.cancel"
		if reject {
			status, action = apRejected, "ai.proposal.reject"
		} else if who.Name != p.ProposedBy && !who.Role.AtLeast(authn.Admin) {
			writeError(w, http.StatusForbidden, "only the person who proposed it, or an administrator, can cancel it")
			return
		}
		now := time.Now().UTC()
		p.Status, p.DecidedBy, p.DecidedAt = status, who.Name, &now
		if err := s.docs.Put(ctx, kindAIProposal, p.ID, p); err != nil {
			s.internalError(w, "saving the decision", err)
			return
		}
		s.auditReq(r, action, "ai-proposal/"+p.ID, p.Title, true)
		writeJSON(w, http.StatusOK, p)
	}
}
